/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/NYTimes/gziphandler"
	"github.com/gorilla/csrf"
	"github.com/gorilla/sessions"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	coreapi "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/simplifypath"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/yaml"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowv1 "k8s.io/test-infra/prow/client/clientset/versioned/typed/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/config/secret"
	"k8s.io/test-infra/prow/deck/jobs"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/git"
	prowgithub "k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/githuboauth"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/plugins/trigger"
	"k8s.io/test-infra/prow/prstatus"
	"k8s.io/test-infra/prow/spyglass"

	// Import standard spyglass viewers

	"k8s.io/test-infra/prow/spyglass/lenses"
	_ "k8s.io/test-infra/prow/spyglass/lenses/buildlog"
	_ "k8s.io/test-infra/prow/spyglass/lenses/coverage"
	_ "k8s.io/test-infra/prow/spyglass/lenses/junit"
	_ "k8s.io/test-infra/prow/spyglass/lenses/metadata"
	_ "k8s.io/test-infra/prow/spyglass/lenses/restcoverage"
)

// Omittable ProwJob fields.
const (
	// Annotations maps to the serialized value of <ProwJob>.Annotations.
	Annotations string = "annotations"
	// Labels maps to the serialized value of <ProwJob>.Labels.
	Labels string = "labels"
	// DecorationConfig maps to the serialized value of <ProwJob>.Spec.DecorationConfig.
	DecorationConfig string = "decoration_config"
	// PodSpec maps to the serialized value of <ProwJob>.Spec.PodSpec.
	PodSpec string = "pod_spec"
)

type options struct {
	configPath            string
	jobConfigPath         string
	buildCluster          string
	kubernetes            prowflagutil.KubernetesOptions
	github                prowflagutil.GitHubOptions
	tideURL               string
	hookURL               string
	oauthURL              string
	githubOAuthConfigFile string
	cookieSecretFile      string
	redirectHTTPTo        string
	hiddenOnly            bool
	pregeneratedData      string
	staticFilesLocation   string
	templateFilesLocation string
	showHidden            bool
	spyglass              bool
	spyglassFilesLocation string
	gcsCredentialsFile    string
	rerunCreatesJob       bool
	allowInsecure         bool
	dryRun                bool
	pluginConfig          string
}

func (o *options) Validate() error {
	if err := o.kubernetes.Validate(false); err != nil {
		return err
	}
	if err := o.github.Validate(o.dryRun); err != nil {
		return err
	}

	if o.configPath == "" {
		return errors.New("required flag --config-path was unset")
	}

	// TODO(Katharine): remove this handling after 2019-10-31
	// We used to set a default value for --cookie-secret-file, but we also have code that
	// assumes we don't. If it's not set, but it is required that it is, and a file exists
	// at the old default, we set it back to that default and emit an error.
	if o.cookieSecretFile == "" && o.oauthURL != "" {
		if _, err := os.Stat("/etc/cookie/secret"); err == nil {
			o.cookieSecretFile = "/etc/cookie/secret"
			logrus.Error("You haven't set --cookie-secret, but you're assuming it is set to '/etc/cookie/secret'. Add --cookie-secret=/etc/cookie/secret to your deck instance's arguments. Your configuration will stop working at the end of October 2019.")
		}
	}

	if o.oauthURL != "" {
		if o.githubOAuthConfigFile == "" {
			return errors.New("an OAuth URL was provided but required flag --github-oauth-config-file was unset")
		}
		if o.cookieSecretFile == "" {
			return errors.New("an OAuth URL was provided but required flag --cookie-secret was unset")
		}
	}

	if o.hiddenOnly && o.showHidden {
		return errors.New("'--hidden-only' and '--show-hidden' are mutually exclusive, the first one shows only hidden job, the second one shows both hidden and non-hidden jobs")
	}
	return nil
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	var o options
	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")
	fs.StringVar(&o.tideURL, "tide-url", "", "Path to tide. If empty, do not serve tide data.")
	fs.StringVar(&o.hookURL, "hook-url", "", "Path to hook plugin help endpoint.")
	fs.StringVar(&o.oauthURL, "oauth-url", "", "Path to deck user dashboard endpoint.")
	fs.StringVar(&o.githubOAuthConfigFile, "github-oauth-config-file", "/etc/github/secret", "Path to the file containing the GitHub App Client secret.")
	fs.StringVar(&o.cookieSecretFile, "cookie-secret", "", "Path to the file containing the cookie secret key.")
	// use when behind a load balancer
	fs.StringVar(&o.redirectHTTPTo, "redirect-http-to", "", "Host to redirect http->https to based on x-forwarded-proto == http.")
	// use when behind an oauth proxy
	fs.BoolVar(&o.hiddenOnly, "hidden-only", false, "Show only hidden jobs. Useful for serving hidden jobs behind an oauth proxy.")
	fs.StringVar(&o.pregeneratedData, "pregenerated-data", "", "Use API output from another prow instance. Used by the prow/cmd/deck/runlocal script")
	fs.BoolVar(&o.showHidden, "show-hidden", false, "Show all jobs, including hidden ones")
	fs.BoolVar(&o.spyglass, "spyglass", false, "Use Prow built-in job viewing instead of Gubernator")
	fs.StringVar(&o.spyglassFilesLocation, "spyglass-files-location", "/lenses", "Location of the static files for spyglass.")
	fs.StringVar(&o.staticFilesLocation, "static-files-location", "/static", "Path to the static files")
	fs.StringVar(&o.templateFilesLocation, "template-files-location", "/template", "Path to the template files")
	fs.StringVar(&o.gcsCredentialsFile, "gcs-credentials-file", "", "Path to the GCS credentials file")
	fs.BoolVar(&o.rerunCreatesJob, "rerun-creates-job", false, "Change the re-run option in Deck to actually create the job. **WARNING:** Only use this with non-public deck instances, otherwise strangers can DOS your Prow instance")
	fs.BoolVar(&o.allowInsecure, "allow-insecure", false, "Allows insecure requests for CSRF and GitHub oauth.")
	fs.BoolVar(&o.dryRun, "dry-run", false, "Whether or not to make mutating API calls to GitHub.")
	fs.StringVar(&o.pluginConfig, "plugin-config", "", "Path to plugin config file, probably /etc/plugins/plugins.yaml")
	o.kubernetes.AddFlags(fs)
	o.github.AddFlagsWithoutDefaultGitHubTokenPath(fs)
	fs.Parse(args)
	o.configPath = config.ConfigPath(o.configPath)
	return o
}

func staticHandlerFromDir(dir string) http.Handler {
	return gziphandler.GzipHandler(handleCached(http.FileServer(http.Dir(dir))))
}

var (
	deckMetrics = struct {
		httpRequestDuration *prometheus.HistogramVec
		httpResponseSize    *prometheus.HistogramVec
	}{
		httpRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "deck_http_request_duration_seconds",
				Help:    "http request duration in seconds",
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20},
			},
			[]string{"path", "method", "status", "user_agent"},
		),
		httpResponseSize: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "deck_http_response_size_bytes",
				Help:    "http response size in bytes",
				Buckets: []float64{16384, 32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 4194304, 8388608, 16777216, 33554432},
			},
			[]string{"path", "method", "status", "user_agent"},
		),
	}
)

type authCfgGetter func(*prowapi.Refs) prowapi.RerunAuthConfig

type traceResponseWriter struct {
	http.ResponseWriter
	statusCode int
	size       int
}

func (trw *traceResponseWriter) WriteHeader(code int) {
	trw.statusCode = code
	trw.ResponseWriter.WriteHeader(code)
}

func (trw *traceResponseWriter) Write(data []byte) (int, error) {
	size, err := trw.ResponseWriter.Write(data)
	trw.size += size
	return size, err
}

func init() {
	prometheus.MustRegister(deckMetrics.httpRequestDuration)
	prometheus.MustRegister(deckMetrics.httpResponseSize)
}

func traceHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		// Initialize the status to 200 in case WriteHeader is not called
		trw := &traceResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		h.ServeHTTP(trw, r)
		latency := time.Since(t)
		labels := prometheus.Labels{"path": simplifier.Simplify(r.URL.Path), "method": r.Method, "status": strconv.Itoa(trw.statusCode), "user_agent": r.Header.Get("User-Agent")}
		deckMetrics.httpRequestDuration.With(labels).Observe(latency.Seconds())
		deckMetrics.httpResponseSize.With(labels).Observe(float64(trw.size))
	})
}

var simplifier = simplifypath.NewSimplifier(l("", // shadow element mimicing the root
	l("badge.svg"),
	l("command-help"),
	l("config"),
	l("data.js"),
	l("favicon.ico"),
	l("github-login",
		l("redirect")),
	l("job-history",
		v("job")),
	l("log"),
	l("plugin-config"),
	l("plugin-help"),
	l("plugins"),
	l("pr"),
	l("pr-data.js"),
	l("pr-history"),
	l("prowjob"),
	l("prowjobs.js"),
	l("rerun"),
	l("spyglass",
		l("static",
			v("path")),
		l("lens",
			v("lens",
				v("job")),
		)),
	l("static",
		v("path")),
	l("tide"),
	l("tide-history"),
	l("tide-history.js"),
	l("tide.js"),
	l("view",
		v("job")),
))

// l and v keep the tree legible

func l(fragment string, children ...simplifypath.Node) simplifypath.Node {
	return simplifypath.L(fragment, children...)
}

func v(fragment string, children ...simplifypath.Node) simplifypath.Node {
	return simplifypath.V(fragment, children...)
}

func main() {
	logrusutil.ComponentInit("deck")

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	defer interrupts.WaitForGracefulShutdown()
	pjutil.ServePProf()

	// setup config agent, pod log clients etc.
	configAgent := &config.Agent{}
	if err := configAgent.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	var pluginAgent *plugins.ConfigAgent
	if o.pluginConfig != "" {
		pluginAgent = &plugins.ConfigAgent{}
		if err := pluginAgent.Start(o.pluginConfig, false); err != nil {
			logrus.WithError(err).Fatal("Error loading Prow plugin config.")
		}
	} else {
		logrus.Info("No plugins configuration was provided to deck. You must provide one to reuse /test checks for rerun")
	}

	metrics.ExposeMetrics("deck", cfg().PushGateway)

	// signal to the world that we are healthy
	// this needs to be in a separate port as we don't start the
	// main server with the main mux until we're ready
	health := pjutil.NewHealth()

	mux := http.NewServeMux()
	// setup common handlers for local and deployed runs
	mux.Handle("/static/", http.StripPrefix("/static", staticHandlerFromDir(o.staticFilesLocation)))
	mux.Handle("/config", gziphandler.GzipHandler(handleConfig(cfg, logrus.WithField("handler", "/config"))))
	mux.Handle("/plugin-config", gziphandler.GzipHandler(handlePluginConfig(pluginAgent, logrus.WithField("handler", "/plugin-config"))))
	mux.Handle("/favicon.ico", gziphandler.GzipHandler(handleFavicon(o.staticFilesLocation, cfg)))

	// Set up handlers for template pages.
	mux.Handle("/pr", gziphandler.GzipHandler(handleSimpleTemplate(o, cfg, "pr.html", nil)))
	mux.Handle("/command-help", gziphandler.GzipHandler(handleSimpleTemplate(o, cfg, "command-help.html", nil)))
	mux.Handle("/plugin-help", http.RedirectHandler("/command-help", http.StatusMovedPermanently))
	mux.Handle("/tide", gziphandler.GzipHandler(handleSimpleTemplate(o, cfg, "tide.html", nil)))
	mux.Handle("/tide-history", gziphandler.GzipHandler(handleSimpleTemplate(o, cfg, "tide-history.html", nil)))
	mux.Handle("/plugins", gziphandler.GzipHandler(handleSimpleTemplate(o, cfg, "plugins.html", nil)))

	runLocal := o.pregeneratedData != ""

	var fallbackHandler func(http.ResponseWriter, *http.Request)
	if runLocal {
		localDataHandler := staticHandlerFromDir(o.pregeneratedData)
		fallbackHandler = localDataHandler.ServeHTTP
	} else {
		fallbackHandler = http.NotFound
	}

	authCfgGetter := func(refs *prowapi.Refs) prowapi.RerunAuthConfig {
		return cfg().Deck.RerunAuthConfigs.GetRerunAuthConfig(refs)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fallbackHandler(w, r)
			return
		}
		indexHandler := handleSimpleTemplate(o, cfg, "index.html", struct {
			SpyglassEnabled bool
			ReRunCreatesJob bool
			AllowAnyone     bool
		}{
			SpyglassEnabled: o.spyglass,
			ReRunCreatesJob: o.rerunCreatesJob,
			AllowAnyone:     authCfgGetter(nil).AllowAnyone})
		indexHandler(w, r)
	})

	if runLocal {
		mux = localOnlyMain(cfg, o, mux)
	} else {
		mux = prodOnlyMain(cfg, pluginAgent, authCfgGetter, o, mux)
	}

	// signal to the world that we're ready
	health.ServeReady()

	// cookie secret will be used for CSRF protection and should be exactly 32 bytes
	// we sometimes accept different lengths to stay backwards compatible
	var csrfToken []byte
	if o.cookieSecretFile != "" {
		cookieSecretRaw, err := loadToken(o.cookieSecretFile)
		if err != nil {
			logrus.WithError(err).Fatal("Could not read cookie secret file")
		}
		decodedSecret, err := base64.StdEncoding.DecodeString(string(cookieSecretRaw))
		if err != nil {
			logrus.WithError(err).Fatal("Error decoding cookie secret")
		}
		if len(decodedSecret) == 32 {
			csrfToken = decodedSecret
		}
		if len(decodedSecret) > 32 {
			logrus.Warning("Cookie secret should be exactly 32 bytes. Consider truncating the existing cookie to that length")
			hash := sha256.Sum256(decodedSecret)
			csrfToken = hash[:]
		}
		if len(decodedSecret) < 32 {
			if o.rerunCreatesJob {
				logrus.Fatal("Cookie secret must be exactly 32 bytes")
				return
			}
			logrus.Warning("Cookie secret should be exactly 32 bytes")
		}
	}

	// if we allow direct reruns, we must protect against CSRF in all post requests using the cookie secret as a token
	// for more information about CSRF, see https://github.com/kubernetes/test-infra/blob/master/prow/cmd/deck/csrf.md
	if o.rerunCreatesJob && csrfToken == nil && !authCfgGetter(nil).AllowAnyone {
		logrus.Fatal("Rerun creates job cannot be enabled without CSRF protection, which requires --cookie-secret to be exactly 32 bytes")
		return
	}

	if csrfToken != nil {
		CSRF := csrf.Protect(csrfToken, csrf.Path("/"), csrf.Secure(!o.allowInsecure))
		logrus.WithError(http.ListenAndServe(":8080", CSRF(traceHandler(mux)))).Fatal("ListenAndServe returned.")
		return
	}
	// setup done, actually start the server
	server := &http.Server{Addr: ":8080", Handler: traceHandler(mux)}
	interrupts.ListenAndServe(server, 5*time.Second)
}

// localOnlyMain contains logic used only when running locally, and is mutually exclusive with
// prodOnlyMain.
func localOnlyMain(cfg config.Getter, o options, mux *http.ServeMux) *http.ServeMux {
	mux.Handle("/github-login", gziphandler.GzipHandler(handleSimpleTemplate(o, cfg, "github-login.html", nil)))

	if o.spyglass {
		initSpyglass(cfg, o, mux, nil, nil, nil)
	}

	return mux
}

type podLogClient struct {
	client corev1.PodInterface
}

func (c *podLogClient) GetLogs(name string, opts *coreapi.PodLogOptions) ([]byte, error) {
	reader, err := c.client.GetLogs(name, &coreapi.PodLogOptions{Container: kube.TestContainerName}).Stream()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return ioutil.ReadAll(reader)
}

type pjListingClient interface {
	List(context.Context, *prowapi.ProwJobList, ...ctrlruntimeclient.ListOption) error
}

type filteringProwJobLister struct {
	ctx         context.Context
	client      pjListingClient
	hiddenRepos func() sets.String
	hiddenOnly  bool
	showHidden  bool
}

func (c *filteringProwJobLister) ListProwJobs(selector string) ([]prowapi.ProwJob, error) {
	prowJobList := &prowapi.ProwJobList{}
	parsedSelector, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse selector: %v", err)
	}
	listOpts := &ctrlruntimeclient.ListOptions{LabelSelector: parsedSelector}
	if err := c.client.List(c.ctx, prowJobList, listOpts); err != nil {
		return nil, err
	}

	var filtered []prowapi.ProwJob
	for _, item := range prowJobList.Items {
		shouldHide := item.Spec.Hidden || c.pjHasHiddenRefs(item)
		if shouldHide && c.showHidden {
			filtered = append(filtered, item)
		} else if shouldHide == c.hiddenOnly {
			// this is a hidden job, show it if we're asked
			// to only show hidden jobs otherwise hide it
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func (c *filteringProwJobLister) pjHasHiddenRefs(pj prowapi.ProwJob) bool {
	allRefs := pj.Spec.ExtraRefs
	if pj.Spec.Refs != nil {
		allRefs = append(allRefs, *pj.Spec.Refs)
	}
	for _, refs := range allRefs {
		if c.hiddenRepos().HasAny(fmt.Sprintf("%s/%s", refs.Org, refs.Repo), refs.Org) {
			return true
		}
	}

	return false
}

type pjListingClientWrapper struct {
	reader ctrlruntimeclient.Reader
}

func (w *pjListingClientWrapper) List(
	ctx context.Context,
	pjl *prowapi.ProwJobList,
	opts ...ctrlruntimeclient.ListOption) error {
	return w.reader.List(ctx, pjl, opts...)
}

// prodOnlyMain contains logic only used when running deployed, not locally
func prodOnlyMain(cfg config.Getter, pluginAgent *plugins.ConfigAgent, authCfgGetter authCfgGetter, o options, mux *http.ServeMux) *http.ServeMux {
	prowJobClient, err := o.kubernetes.ProwJobClient(cfg().ProwJobNamespace, false)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting ProwJob client for infrastructure cluster.")
	}
	restCfg, err := o.kubernetes.InfrastructureClusterConfig(false)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting infrastructure cluster config.")
	}
	mgr, err := manager.New(restCfg, manager.Options{
		Namespace:          cfg().ProwJobNamespace,
		MetricsBindAddress: "0",
		LeaderElection:     false},
	)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting manager.")
	}
	go func() {
		if err := mgr.Start(make(chan struct{})); err != nil {
			logrus.WithError(err).Fatal("Error starting manager.")
		} else {
			logrus.Info("Manager stopped gracefully.")
		}
	}()

	buildClusterClients, err := o.kubernetes.BuildClusterClients(cfg().PodNamespace, false)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting Kubernetes client.")
	}

	podLogClients := map[string]jobs.PodLogClient{}
	for clusterContext, client := range buildClusterClients {
		podLogClients[clusterContext] = &podLogClient{client: client}
	}

	ja := jobs.NewJobAgent(&filteringProwJobLister{
		client: &pjListingClientWrapper{mgr.GetClient()},
		hiddenRepos: func() sets.String {
			return sets.NewString(cfg().Deck.HiddenRepos...)
		},
		hiddenOnly: o.hiddenOnly,
		showHidden: o.showHidden,
	}, podLogClients, cfg)
	ja.Start()

	// setup prod only handlers
	mux.Handle("/data.js", gziphandler.GzipHandler(handleData(ja, logrus.WithField("handler", "/data.js"))))
	mux.Handle("/prowjobs.js", gziphandler.GzipHandler(handleProwJobs(ja, logrus.WithField("handler", "/prowjobs.js"))))
	mux.Handle("/badge.svg", gziphandler.GzipHandler(handleBadge(ja)))
	mux.Handle("/log", gziphandler.GzipHandler(handleLog(ja, logrus.WithField("handler", "/log"))))

	mux.Handle("/prowjob", gziphandler.GzipHandler(handleProwJob(prowJobClient, logrus.WithField("handler", "/prowjob"))))

	// We use the GH client to resolve GH teams when determining who is permitted to rerun a job.
	// When inrepoconfig is enabled, both the GitHubClient and the gitClient are used to resolve
	// presubmits dynamically which we need for the PR history page.
	var githubClient deckGitHubClient
	var gitClient *git.Client
	secretAgent := &secret.Agent{}
	if o.github.TokenPath != "" {
		if err := secretAgent.Start([]string{o.github.TokenPath}); err != nil {
			logrus.WithError(err).Fatal("Error starting secrets agent.")
		}
		githubClient, err = o.github.GitHubClient(secretAgent, o.dryRun)
		if err != nil {
			logrus.WithError(err).Fatal("Error getting GitHub client.")
		}
		gitClient, err = o.github.GitClient(secretAgent, o.dryRun)
		if err != nil {
			logrus.WithError(err).Fatal("Error getting Git client.")
		}
	}

	if o.spyglass {
		initSpyglass(cfg, o, mux, ja, githubClient, gitClient)
	}

	if o.hookURL != "" {
		mux.Handle("/plugin-help.js",
			gziphandler.GzipHandler(handlePluginHelp(newHelpAgent(o.hookURL), logrus.WithField("handler", "/plugin-help.js"))))
	}

	if o.tideURL != "" {
		ta := &tideAgent{
			log:  logrus.WithField("agent", "tide"),
			path: o.tideURL,
			updatePeriod: func() time.Duration {
				return cfg().Deck.TideUpdatePeriod.Duration
			},
			hiddenRepos: func() []string {
				return cfg().Deck.HiddenRepos
			},
			hiddenOnly: o.hiddenOnly,
			showHidden: o.showHidden,
		}
		ta.start()
		mux.Handle("/tide.js", gziphandler.GzipHandler(handleTidePools(cfg, ta, logrus.WithField("handler", "/tide.js"))))
		mux.Handle("/tide-history.js", gziphandler.GzipHandler(handleTideHistory(ta, logrus.WithField("handler", "/tide-history.js"))))
	}

	// Enable Git OAuth feature if oauthURL is provided.
	var goa *githuboauth.Agent
	if o.oauthURL != "" {
		githubOAuthConfigRaw, err := loadToken(o.githubOAuthConfigFile)
		if err != nil {
			logrus.WithError(err).Fatal("Could not read github oauth config file.")
		}

		cookieSecretRaw, err := loadToken(o.cookieSecretFile)
		if err != nil {
			logrus.WithError(err).Fatal("Could not read cookie secret file.")
		}

		var githubOAuthConfig githuboauth.Config
		if err := yaml.Unmarshal(githubOAuthConfigRaw, &githubOAuthConfig); err != nil {
			logrus.WithError(err).Fatal("Error unmarshalling github oauth config")
		}
		if !isValidatedGitOAuthConfig(&githubOAuthConfig) {
			logrus.Fatal("Error invalid github oauth config")
		}

		decodedSecret, err := base64.StdEncoding.DecodeString(string(cookieSecretRaw))
		if err != nil {
			logrus.WithError(err).Fatal("Error decoding cookie secret")
		}
		if len(decodedSecret) == 0 {
			logrus.Fatal("Cookie secret should not be empty")
		}
		cookie := sessions.NewCookieStore(decodedSecret)
		githubOAuthConfig.InitGitHubOAuthConfig(cookie)

		goa = githuboauth.NewAgent(&githubOAuthConfig, logrus.WithField("client", "githuboauth"))
		oauthClient := o.github.GitHubOAuthClient(&oauth2.Config{
			ClientID:     githubOAuthConfig.ClientID,
			ClientSecret: githubOAuthConfig.ClientSecret,
			RedirectURL:  githubOAuthConfig.RedirectURL,
			Scopes:       githubOAuthConfig.Scopes,
		})

		repos := cfg().AllRepos.List()

		prStatusAgent := prstatus.NewDashboardAgent(
			repos,
			&githubOAuthConfig,
			&o.github,
			logrus.WithField("client", "pr-status"))

		secure := !o.allowInsecure

		mux.Handle("/pr-data.js", handleNotCached(
			prStatusAgent.HandlePrStatus(prStatusAgent)))
		// Handles login request.
		mux.Handle("/github-login", goa.HandleLogin(oauthClient, secure))
		// Handles redirect from GitHub OAuth server.
		mux.Handle("/github-login/redirect", goa.HandleRedirect(oauthClient, &o.github, secure))
	}

	mux.Handle("/rerun", gziphandler.GzipHandler(handleRerun(prowJobClient, o.rerunCreatesJob, authCfgGetter, goa, &o.github, githubClient, pluginAgent, logrus.WithField("handler", "/rerun"))))

	// optionally inject http->https redirect handler when behind loadbalancer
	if o.redirectHTTPTo != "" {
		redirectMux := http.NewServeMux()
		redirectMux.Handle("/", func(oldMux *http.ServeMux, host string) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-forwarded-proto") == "http" {
					redirectURL, err := url.Parse(r.URL.String())
					if err != nil {
						logrus.Errorf("Failed to parse URL: %s.", r.URL.String())
						http.Error(w, "Failed to perform https redirect.", http.StatusInternalServerError)
						return
					}
					redirectURL.Scheme = "https"
					redirectURL.Host = host
					http.Redirect(w, r, redirectURL.String(), http.StatusMovedPermanently)
				} else {
					oldMux.ServeHTTP(w, r)
				}
			}
		}(mux, o.redirectHTTPTo))
		mux = redirectMux
	}

	return mux
}

func initSpyglass(cfg config.Getter, o options, mux *http.ServeMux, ja *jobs.JobAgent, gitHubClient deckGitHubClient, gitClient *git.Client) {
	var c *storage.Client
	var err error
	if o.gcsCredentialsFile == "" {
		c, err = storage.NewClient(context.Background(), option.WithoutAuthentication())
	} else {
		c, err = storage.NewClient(context.Background(), option.WithCredentialsFile(o.gcsCredentialsFile))
	}
	if err != nil {
		logrus.WithError(err).Fatal("Error getting GCS client")
	}
	sg := spyglass.New(ja, cfg, c, o.gcsCredentialsFile, context.Background())
	sg.Start()

	mux.Handle("/spyglass/static/", http.StripPrefix("/spyglass/static", staticHandlerFromDir(o.spyglassFilesLocation)))
	mux.Handle("/spyglass/lens/", gziphandler.GzipHandler(http.StripPrefix("/spyglass/lens/", handleArtifactView(o, sg, cfg))))
	mux.Handle("/view/", gziphandler.GzipHandler(handleRequestJobViews(sg, cfg, o, logrus.WithField("handler", "/view"))))
	mux.Handle("/job-history/", gziphandler.GzipHandler(handleJobHistory(o, cfg, c, logrus.WithField("handler", "/job-history"))))
	mux.Handle("/pr-history/", gziphandler.GzipHandler(handlePRHistory(o, cfg, c, gitHubClient, gitClient, logrus.WithField("handler", "/pr-history"))))
}

func loadToken(file string) ([]byte, error) {
	raw, err := ioutil.ReadFile(file)
	if err != nil {
		return []byte{}, err
	}
	return bytes.TrimSpace(raw), nil
}

// copy a http.Request
// see: https://go-review.googlesource.com/c/go/+/36483/3/src/net/http/server.go
func dupeRequest(original *http.Request) *http.Request {
	r2 := new(http.Request)
	*r2 = *original
	r2.URL = new(url.URL)
	*r2.URL = *original.URL
	return r2
}

func handleCached(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This looks ridiculous but actually no-cache means "revalidate" and
		// "max-age=0" just means there is no time in which it can skip
		// revalidation. We also need to set must-revalidate because no-cache
		// doesn't imply must-revalidate when using the back button
		// https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.9.1
		// TODO: consider setting a longer max-age
		// setting it this way means the content is always revalidated
		w.Header().Set("Cache-Control", "public, max-age=0, no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

func setHeadersNoCaching(w http.ResponseWriter) {
	// Note that we need to set both no-cache and no-store because only some
	// browsers decided to (incorrectly) treat no-cache as "never store"
	// IE "no-store". for good measure to cover older browsers we also set
	// expires and pragma: https://stackoverflow.com/a/2068407
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func writeJSONResponse(w http.ResponseWriter, r *http.Request, d []byte) {
	// If we have a "var" query, then write out "var value = {...};".
	// Otherwise, just write out the JSON.
	if v := r.URL.Query().Get("var"); v != "" {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, "var %s = %s;", v, string(d))
	} else {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(d))
	}
}

func handleNotCached(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		next.ServeHTTP(w, r)
	}
}

func handleProwJobs(ja *jobs.JobAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		jobs := ja.ProwJobs()
		omit := r.URL.Query().Get("omit")

		if set := sets.NewString(strings.Split(omit, ",")...); set.Len() > 0 {
			for i := range jobs {
				if set.Has(Annotations) {
					jobs[i].Annotations = nil
				}
				if set.Has(Labels) {
					jobs[i].Labels = nil
				}
				if set.Has(DecorationConfig) {
					jobs[i].Spec.DecorationConfig = nil
				}
				if set.Has(PodSpec) {
					jobs[i].Spec.PodSpec = nil
				}
			}
		}

		jd, err := json.Marshal(struct {
			Items []prowapi.ProwJob `json:"items"`
		}{jobs})
		if err != nil {
			log.WithError(err).Error("Error marshaling jobs.")
			jd = []byte("{}")
		}
		writeJSONResponse(w, r, jd)
	}
}

func handleData(ja *jobs.JobAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		jobs := ja.Jobs()
		jd, err := json.Marshal(jobs)
		if err != nil {
			log.WithError(err).Error("Error marshaling jobs.")
			jd = []byte("[]")
		}
		writeJSONResponse(w, r, jd)
	}
}

// handleBadge handles requests to get a badge for one or more jobs
// The url must look like this, where `jobs` is a comma-separated
// list of globs:
//
// /badge.svg?jobs=<glob>[,<glob2>]
//
// Examples:
// - /badge.svg?jobs=pull-kubernetes-bazel-build
// - /badge.svg?jobs=pull-kubernetes-*
// - /badge.svg?jobs=pull-kubernetes-e2e*,pull-kubernetes-*,pull-kubernetes-integration-*
func handleBadge(ja *jobs.JobAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		wantJobs := r.URL.Query().Get("jobs")
		if wantJobs == "" {
			http.Error(w, "missing jobs query parameter", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")

		allJobs := ja.ProwJobs()
		_, _, svg := renderBadge(pickLatestJobs(allJobs, wantJobs))
		w.Write(svg)
	}
}

// handleJobHistory handles requests to get the history of a given job
// The url must look like this for presubmits:
//
// /job-history/<gcs-bucket-name>/pr-logs/directory/<job-name>
//
// Example:
// - /job-history/kubernetes-jenkins/pr-logs/directory/pull-test-infra-verify-gofmt
//
// For periodics or postsubmits, the url must look like this:
//
// /job-history/<gcs-bucket-name>/logs/<job-name>
//
// Example:
// - /job-history/kubernetes-jenkins/logs/ci-kubernetes-e2e-prow-canary
func handleJobHistory(o options, cfg config.Getter, gcsClient *storage.Client, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		tmpl, err := getJobHistory(r.URL, cfg(), gcsClient)
		if err != nil {
			msg := fmt.Sprintf("failed to get job history: %v", err)
			log.WithField("url", r.URL.String()).Error(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		handleSimpleTemplate(o, cfg, "job-history.html", tmpl)(w, r)
	}
}

// handlePRHistory handles requests to get the test history if a given PR
// The url must look like this:
//
// /pr-history?org=<org>&repo=<repo>&pr=<pr number>
func handlePRHistory(o options, cfg config.Getter, gcsClient *storage.Client, gitHubClient deckGitHubClient, gitClient *git.Client, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		tmpl, err := getPRHistory(r.URL, cfg(), gcsClient, gitHubClient, gitClient)
		if err != nil {
			msg := fmt.Sprintf("failed to get PR history: %v", err)
			log.WithField("url", r.URL.String()).Info(msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}
		handleSimpleTemplate(o, cfg, "pr-history.html", tmpl)(w, r)
	}
}

// handleRequestJobViews handles requests to get all available artifact views for a given job.
// The url must specify a storage key type, such as "prowjob" or "gcs":
//
// /view/<key-type>/<key>
//
// Examples:
// - /view/gcs/kubernetes-jenkins/pr-logs/pull/test-infra/9557/pull-test-infra-verify-gofmt/15688/
// - /view/prowjob/echo-test/1046875594609922048
func handleRequestJobViews(sg *spyglass.Spyglass, cfg config.Getter, o options, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		setHeadersNoCaching(w)
		src := strings.TrimPrefix(r.URL.Path, "/view/")

		csrfToken := csrf.Token(r)
		page, err := renderSpyglass(sg, cfg, src, o, csrfToken, log)
		if err != nil {
			log.WithError(err).Error("error rendering spyglass page")
			message := fmt.Sprintf("error rendering spyglass page: %v", err)
			http.Error(w, message, http.StatusInternalServerError)
			return
		}

		fmt.Fprint(w, page)
		elapsed := time.Since(start)
		log.WithFields(logrus.Fields{
			"duration": elapsed.String(),
			"endpoint": r.URL.Path,
			"source":   src,
		}).Info("Loading view completed.")
	}
}

// renderSpyglass returns a pre-rendered Spyglass page from the given source string
func renderSpyglass(sg *spyglass.Spyglass, cfg config.Getter, src string, o options, csrfToken string, log *logrus.Entry) (string, error) {
	renderStart := time.Now()

	src = strings.TrimSuffix(src, "/")
	realPath, err := sg.ResolveSymlink(src)
	if err != nil {
		return "", fmt.Errorf("error when resolving real path: %v", err)
	}
	src = realPath

	artifactNames, err := sg.ListArtifacts(src)
	if err != nil {
		return "", fmt.Errorf("error listing artifacts: %v", err)
	}
	if len(artifactNames) == 0 {
		return "", fmt.Errorf("found no artifacts for %s", src)
	}

	regexCache := cfg().Deck.Spyglass.RegexCache
	lensCache := map[int][]string{}
	var lensIndexes []int
lensesLoop:
	for i, lfc := range cfg().Deck.Spyglass.Lenses {
		matches := map[string]struct{}{}
		for _, re := range lfc.RequiredFiles {
			found := false
			for _, a := range artifactNames {
				if regexCache[re].MatchString(a) {
					matches[a] = struct{}{}
					found = true
				}
			}
			if !found {
				continue lensesLoop
			}
		}

		for _, re := range lfc.OptionalFiles {
			for _, a := range artifactNames {
				if regexCache[re].MatchString(a) {
					matches[a] = struct{}{}
				}
			}
		}

		matchSlice := make([]string, 0, len(matches))
		for k := range matches {
			matchSlice = append(matchSlice, k)
		}

		lensCache[i] = matchSlice
		lensIndexes = append(lensIndexes, i)
	}

	lensIndexes, ls := sg.Lenses(lensIndexes)

	jobHistLink := ""
	jobPath, err := sg.JobPath(src)
	if err == nil {
		jobHistLink = path.Join("/job-history", jobPath)
	}

	var prowJobLink string
	prowJobName, err := sg.ProwJobName(src)
	if err == nil {
		if prowJobName != "" {
			u, err := url.Parse("/prowjob")
			if err != nil {
				return "", fmt.Errorf("error parsing prowjob path: %v", err)
			}
			query := url.Values{}
			query.Set("prowjob", prowJobName)
			u.RawQuery = query.Encode()
			prowJobLink = u.String()
		}
	} else {
		log.WithError(err).Warningf("Error getting ProwJob name for source %q.", src)
	}

	artifactsLink := ""
	gcswebPrefix := cfg().Deck.Spyglass.GCSBrowserPrefix
	if gcswebPrefix != "" {
		runPath, err := sg.RunPath(src)
		if err == nil {
			artifactsLink = gcswebPrefix + runPath
			// gcsweb wants us to end URLs with a trailing slash
			if !strings.HasSuffix(artifactsLink, "/") {
				artifactsLink += "/"
			}
		}
	}

	prHistLink := ""
	org, repo, number, err := sg.RunToPR(src)
	if err == nil {
		prHistLink = "/pr-history?org=" + org + "&repo=" + repo + "&pr=" + strconv.Itoa(number)
	}

	jobName, buildID, err := sg.KeyToJob(src)
	if err != nil {
		return "", fmt.Errorf("error determining jobName / buildID: %v", err)
	}

	prLink := ""
	j, err := sg.JobAgent.GetProwJob(jobName, buildID)
	if err == nil && j.Spec.Refs != nil && len(j.Spec.Refs.Pulls) > 0 {
		prLink = j.Spec.Refs.Pulls[0].Link
	}

	announcement := ""
	if cfg().Deck.Spyglass.Announcement != "" {
		announcementTmpl, err := template.New("announcement").Parse(cfg().Deck.Spyglass.Announcement)
		if err != nil {
			return "", fmt.Errorf("error parsing announcement template: %v", err)
		}
		runPath, err := sg.RunPath(src)
		if err != nil {
			runPath = ""
		}
		var announcementBuf bytes.Buffer
		err = announcementTmpl.Execute(&announcementBuf, struct {
			ArtifactPath string
		}{
			ArtifactPath: runPath,
		})
		if err != nil {
			return "", fmt.Errorf("error executing announcement template: %v", err)
		}
		announcement = announcementBuf.String()
	}

	tgLink, err := sg.TestGridLink(src)
	if err != nil {
		tgLink = ""
	}

	extraLinks, err := sg.ExtraLinks(src)
	if err != nil {
		log.WithError(err).WithField("page", src).Warn("Failed to fetch extra links")
		extraLinks = nil
	}

	var viewBuf bytes.Buffer
	type lensesTemplate struct {
		Lenses        map[int]lenses.Lens
		LensIndexes   []int
		Source        string
		LensArtifacts map[int][]string
		JobHistLink   string
		ProwJobLink   string
		ArtifactsLink string
		PRHistLink    string
		Announcement  template.HTML
		TestgridLink  string
		JobName       string
		BuildID       string
		PRLink        string
		ExtraLinks    []spyglass.ExtraLink
	}
	lTmpl := lensesTemplate{
		Lenses:        ls,
		LensIndexes:   lensIndexes,
		Source:        src,
		LensArtifacts: lensCache,
		JobHistLink:   jobHistLink,
		ProwJobLink:   prowJobLink,
		ArtifactsLink: artifactsLink,
		PRHistLink:    prHistLink,
		Announcement:  template.HTML(announcement),
		TestgridLink:  tgLink,
		JobName:       jobName,
		BuildID:       buildID,
		PRLink:        prLink,
		ExtraLinks:    extraLinks,
	}
	t := template.New("spyglass.html")

	if _, err := prepareBaseTemplate(o, cfg, csrfToken, t); err != nil {
		return "", fmt.Errorf("error preparing base template: %v", err)
	}
	t, err = t.ParseFiles(path.Join(o.templateFilesLocation, "spyglass.html"))
	if err != nil {
		return "", fmt.Errorf("error parsing template: %v", err)
	}

	if err = t.Execute(&viewBuf, lTmpl); err != nil {
		return "", fmt.Errorf("error rendering template: %v", err)
	}
	renderElapsed := time.Since(renderStart)
	log.WithFields(logrus.Fields{
		"duration": renderElapsed.String(),
		"source":   src,
	}).Info("Rendered spyglass views.")
	return viewBuf.String(), nil
}

// handleArtifactView handles requests to load a single view for a job. This is what viewers
// will use to call back to themselves.
// Query params:
// - name: required, specifies the name of the viewer to load
// - src: required, specifies the job source from which to fetch artifacts
func handleArtifactView(o options, sg *spyglass.Spyglass, cfg config.Getter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		pathSegments := strings.Split(r.URL.Path, "/")
		if len(pathSegments) != 2 {
			http.NotFound(w, r)
			return
		}
		lensName := pathSegments[0]
		resource := pathSegments[1]

		lens, err := lenses.GetLens(lensName)
		if err != nil {
			http.Error(w, fmt.Sprintf("No such template: %s (%v)", lensName, err), http.StatusNotFound)
			return
		}

		lensConfig := lens.Config()
		lensResourcesDir := lenses.ResourceDirForLens(o.spyglassFilesLocation, lensConfig.Name)

		reqString := r.URL.Query().Get("req")
		var request spyglass.LensRequest
		err = json.Unmarshal([]byte(reqString), &request)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse request: %v", err), http.StatusBadRequest)
			return
		}

		artifacts, err := sg.FetchArtifacts(request.Source, "", cfg().Deck.Spyglass.SizeLimit, request.Artifacts)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to retrieve expected artifacts: %v", err), http.StatusInternalServerError)
			return
		}

		switch resource {
		case "iframe":
			t, err := template.ParseFiles(path.Join(o.templateFilesLocation, "spyglass-lens.html"))
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to load template: %v", err), http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "text/html; encoding=utf-8")
			t.Execute(w, struct {
				Title   string
				BaseURL string
				Head    template.HTML
				Body    template.HTML
			}{
				lensConfig.Title,
				"/spyglass/static/" + lensName + "/",
				template.HTML(lens.Header(artifacts, lensResourcesDir, cfg().Deck.Spyglass.Lenses[request.Index].Lens.Config)),
				template.HTML(lens.Body(artifacts, lensResourcesDir, "", cfg().Deck.Spyglass.Lenses[request.Index].Lens.Config)),
			})
		case "rerender":
			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to read body: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; encoding=utf-8")
			w.Write([]byte(lens.Body(artifacts, lensResourcesDir, string(data), cfg().Deck.Spyglass.Lenses[request.Index].Lens.Config)))
		case "callback":
			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to read body: %v", err), http.StatusInternalServerError)
				return
			}
			w.Write([]byte(lens.Callback(artifacts, lensResourcesDir, string(data), cfg().Deck.Spyglass.Lenses[request.Index].Lens.Config)))
		default:
			http.NotFound(w, r)
		}
	}
}

func handleTidePools(cfg config.Getter, ta *tideAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		queryConfigs := ta.filterHiddenQueries(cfg().Tide.Queries)
		queries := make([]string, 0, len(queryConfigs))
		for _, qc := range queryConfigs {
			queries = append(queries, qc.Query())
		}

		ta.Lock()
		pools := ta.pools
		ta.Unlock()

		payload := tidePools{
			Queries:     queries,
			TideQueries: queryConfigs,
			Pools:       pools,
		}
		pd, err := json.Marshal(payload)
		if err != nil {
			log.WithError(err).Error("Error marshaling payload.")
			pd = []byte("{}")
		}
		writeJSONResponse(w, r, pd)
	}
}

func handleTideHistory(ta *tideAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)

		ta.Lock()
		history := ta.history
		ta.Unlock()

		payload := tideHistory{
			History: history,
		}
		pd, err := json.Marshal(payload)
		if err != nil {
			log.WithError(err).Error("Error marshaling payload.")
			pd = []byte("{}")
		}
		writeJSONResponse(w, r, pd)
	}
}

func handlePluginHelp(ha *helpAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		help, err := ha.getHelp()
		if err != nil {
			log.WithError(err).Error("Getting plugin help from hook.")
			help = &pluginhelp.Help{}
		}
		b, err := json.Marshal(*help)
		if err != nil {
			log.WithError(err).Error("Marshaling plugin help.")
			b = []byte("[]")
		}
		writeJSONResponse(w, r, b)
	}
}

type logClient interface {
	GetJobLog(job, id string) ([]byte, error)
}

// TODO(spxtr): Cache, rate limit.
func handleLog(lc logClient, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		job := r.URL.Query().Get("job")
		id := r.URL.Query().Get("id")
		logger := log.WithFields(logrus.Fields{"job": job, "id": id})
		if err := validateLogRequest(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jobLog, err := lc.GetJobLog(job, id)
		if err != nil {
			http.Error(w, fmt.Sprintf("Log not found: %v", err), http.StatusNotFound)
			logger := logger.WithError(err)
			msg := "Log not found."
			if strings.Contains(err.Error(), "PodInitializing") || strings.Contains(err.Error(), "not found") ||
				strings.Contains(err.Error(), "terminated") {
				// PodInitializing is really common and not something
				// that has any actionable items for administrators
				// monitoring logs, so we should log it as information.
				// Similarly, if a user asks us to proxy through logs
				// for a Pod or ProwJob that doesn't exit, it's not
				// something an administrator wants to see in logs.
				logger.Info(msg)
			} else {
				logger.Warning(msg)
			}
			return
		}
		if _, err = w.Write(jobLog); err != nil {
			logger.WithError(err).Warning("Error writing log.")
		}
	}
}

func validateLogRequest(r *http.Request) error {
	job := r.URL.Query().Get("job")
	id := r.URL.Query().Get("id")

	if job == "" {
		return errors.New("request did not provide the 'job' query parameter")
	}
	if id == "" {
		return errors.New("request did not provide the 'id' query parameter")
	}
	return nil
}

func handleProwJob(prowJobClient prowv1.ProwJobInterface, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("prowjob")
		l := log.WithField("prowjob", name)
		if name == "" {
			http.Error(w, "request did not provide the 'prowjob' query parameter", http.StatusBadRequest)
			return
		}

		pj, err := prowJobClient.Get(name, metav1.GetOptions{})
		if err != nil {
			http.Error(w, fmt.Sprintf("ProwJob not found: %v", err), http.StatusNotFound)
			if !kerrors.IsNotFound(err) {
				// admins only care about errors other than not found
				l.WithError(err).Warning("ProwJob not found.")
			}
			return
		}
		handleSerialize(w, "prowjob", pj, l)
	}
}

// canTriggerJob determines whether the given user can trigger any job.
func canTriggerJob(user string, pj prowapi.ProwJob, cfg prowapi.RerunAuthConfig, cli prowgithub.RerunClient, pluginAgent *plugins.ConfigAgent, log *logrus.Entry) (bool, error) {
	auth, err := cfg.IsAuthorized(user, cli)
	if auth {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	jobPermissions := pj.Spec.RerunAuthConfig
	auth, err = jobPermissions.IsAuthorized(user, cli)
	if auth {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if cli == nil {
		log.Warning("No GitHub token was provided, so we cannot retrieve GitHub teams")
		return false, nil
	}

	// If the job is a presubmit and has an associated PR, and a plugin config is provided,
	// do the same checks as for /test
	if pj.Spec.Type == prowapi.PresubmitJob && pj.Spec.Refs != nil && len(pj.Spec.Refs.Pulls) > 0 {
		if pluginAgent == nil {
			log.Info("No plugin config was provided so we cannot check if the user would be allowed to use /test.")
		} else {
			pcfg := pluginAgent.Config()
			pull := pj.Spec.Refs.Pulls[0]
			org := pj.Spec.Refs.Org
			repo := pj.Spec.Refs.Repo
			_, allowed, err := trigger.TrustedPullRequest(cli, pcfg.TriggerFor(org, repo), pull.Author, org, repo, pull.Number, nil)
			return allowed, err
		}
	}
	return false, nil
}

// handleRerun triggers a rerun of the given job if that features is enabled, it receives a
// POST request, and the user has the necessary permissions. Otherwise, it writes the config
// for a new job but does not trigger it.
func handleRerun(prowJobClient prowv1.ProwJobInterface, createProwJob bool, cfg authCfgGetter, goa *githuboauth.Agent, ghc githuboauth.GitHubClientGetter, cli prowgithub.RerunClient, pluginAgent *plugins.ConfigAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("prowjob")
		l := log.WithField("prowjob", name)
		if name == "" {
			http.Error(w, "request did not provide the 'prowjob' query parameter", http.StatusBadRequest)
			return
		}
		pj, err := prowJobClient.Get(name, metav1.GetOptions{})
		if err != nil {
			http.Error(w, fmt.Sprintf("ProwJob not found: %v", err), http.StatusNotFound)
			if !kerrors.IsNotFound(err) {
				// admins only care about errors other than not found
				l.WithError(err).Warning("ProwJob not found.")
			}
			return
		}
		newPJ := pjutil.NewProwJob(pj.Spec, pj.ObjectMeta.Labels, pj.ObjectMeta.Annotations)
		l = l.WithField("job", newPJ.Spec.Job)
		switch r.Method {
		case http.MethodGet:
			handleSerialize(w, "prowjob", newPJ, l)
		case http.MethodPost:
			if !createProwJob {
				http.Error(w, "Direct rerun feature is not enabled. Enable with the '--rerun-creates-job' flag.", http.StatusMethodNotAllowed)
				return
			}
			authConfig := cfg(pj.Spec.Refs)
			var allowed bool
			if authConfig.AllowAnyone || pj.Spec.RerunAuthConfig.AllowAnyone {
				// Skip getting the users login via GH oauth if anyone is allowed to rerun
				// jobs so that GH oauth doesn't need to be set up for private Prows.
				allowed = true
			} else {
				if goa == nil {
					msg := "GitHub oauth must be configured to rerun jobs unless 'allow_anyone: true' is specified."
					http.Error(w, msg, http.StatusInternalServerError)
					l.Error(msg)
					return
				}
				login, err := goa.GetLogin(r, ghc)
				if err != nil {
					l.WithError(err).Errorf("Error retrieving GitHub login")
					http.Error(w, "Error retrieving GitHub login", http.StatusUnauthorized)
					return
				}
				l = l.WithField("user", login)
				allowed, err = canTriggerJob(login, newPJ, authConfig, cli, pluginAgent, l)
				if err != nil {
					http.Error(w, fmt.Sprintf("Error checking if user can trigger job: %v", err), http.StatusInternalServerError)
					l.WithError(err).Errorf("Error checking if user can trigger job")
					return
				}
			}

			l = l.WithField("allowed", allowed)
			l.Info("Attempted rerun")
			if !allowed {
				if _, err = w.Write([]byte("You don't have permission to rerun that job")); err != nil {
					l.WithError(err).Error("Error writing to rerun response.")
				}
				return
			}
			created, err := prowJobClient.Create(&newPJ)
			if err != nil {
				l.WithError(err).Error("Error creating job")
				http.Error(w, fmt.Sprintf("Error creating job: %v", err), http.StatusInternalServerError)
				return
			}
			l = l.WithField("new-prowjob", created.Name)
			l.Info("Successfully created a rerun PJ.")
			if _, err = w.Write([]byte("Job successfully triggered. Wait 30 seconds and refresh the page for the job to show up")); err != nil {
				l.WithError(err).Error("Error writing to rerun response.")
			}
			return
		default:
			http.Error(w, fmt.Sprintf("bad verb %v", r.Method), http.StatusMethodNotAllowed)
			return
		}
	}
}

func handleSerialize(w http.ResponseWriter, name string, data interface{}, l *logrus.Entry) {
	setHeadersNoCaching(w)
	b, err := yaml.Marshal(data)
	if err != nil {
		msg := fmt.Sprintf("Error marshaling %q.", name)
		l.WithError(err).Error(msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	buff := bytes.NewBuffer(b)
	_, err = buff.WriteTo(w)
	if err != nil {
		msg := fmt.Sprintf("Error writing %q.", name)
		l.WithError(err).Error(msg)
		http.Error(w, msg, http.StatusInternalServerError)
	}
}

func handleConfig(cfg config.Getter, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// TODO: add the ability to query for portions of the config?
		handleSerialize(w, "config.yaml", cfg(), log)
	}
}

func handlePluginConfig(pluginAgent *plugins.ConfigAgent, log *logrus.Entry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pluginAgent != nil {
			handleSerialize(w, "plugins.yaml", pluginAgent.Config(), log)
			return
		}
		msg := "Please use the --plugin-config flag to specify the location of the plugin config."
		log.Infof("Could not serve request. %s", msg)
		http.Error(w, msg, http.StatusInternalServerError)
	}
}

func handleFavicon(staticFilesLocation string, cfg config.Getter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		config := cfg()
		if config.Deck.Branding != nil && config.Deck.Branding.Favicon != "" {
			http.ServeFile(w, r, staticFilesLocation+"/"+config.Deck.Branding.Favicon)
		} else {
			http.ServeFile(w, r, staticFilesLocation+"/favicon.ico")
		}
	}
}

func isValidatedGitOAuthConfig(githubOAuthConfig *githuboauth.Config) bool {
	return githubOAuthConfig.ClientID != "" && githubOAuthConfig.ClientSecret != "" &&
		githubOAuthConfig.RedirectURL != ""
}

type deckGitHubClient interface {
	prowgithub.RerunClient
	GetPullRequest(org, repo string, number int) (*prowgithub.PullRequest, error)
	GetRef(org, repo, ref string) (string, error)
}
