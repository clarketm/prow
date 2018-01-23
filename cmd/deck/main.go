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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/pluginhelp"
)

var (
	configPath   = flag.String("config-path", "/etc/config/config", "Path to config.yaml.")
	buildCluster = flag.String("build-cluster", "", "Path to file containing a YAML-marshalled kube.Cluster object. If empty, uses the local cluster.")
	tideURL      = flag.String("tide-url", "", "Path to tide. If empty, do not serve tide data.")
	hookURL      = flag.String("hook-url", "", "Path to hook plugin help endpoint.")
	// use when behind a load balancer
	redirectHTTPTo = flag.String("redirect-http-to", "", "host to redirect http->https to based on x-forwarded-proto == http.")
)

// Matches letters, numbers, hyphens, and underscores.
var objReg = regexp.MustCompile(`^[\w-]+$`)

func main() {
	flag.Parse()
	logrus.SetFormatter(&logrus.JSONFormatter{})
	logger := logrus.WithField("component", "deck")

	configAgent := &config.Agent{}
	if err := configAgent.Start(*configPath); err != nil {
		logger.WithError(err).Fatal("Error starting config agent.")
	}

	kc, err := kube.NewClientInCluster(configAgent.Config().ProwJobNamespace)
	if err != nil {
		logger.WithError(err).Fatal("Error getting client.")
	}
	kc.SetHiddenReposProvider(func() []string { return configAgent.Config().Deck.HiddenRepos })

	var pkcs map[string]*kube.Client
	if *buildCluster == "" {
		pkcs = map[string]*kube.Client{kube.DefaultClusterAlias: kc.Namespace(configAgent.Config().PodNamespace)}
	} else {
		pkcs, err = kube.ClientMapFromFile(*buildCluster, configAgent.Config().PodNamespace)
		if err != nil {
			logger.WithError(err).Fatal("Error getting kube client to build cluster.")
		}
	}
	plClients := map[string]podLogClient{}
	for alias, client := range pkcs {
		plClients[alias] = client
	}

	ja := &JobAgent{
		kc:   kc,
		pkcs: plClients,
		c:    configAgent,
	}
	ja.Start()

	mux := http.NewServeMux()

	mux.Handle("/", gziphandler.GzipHandler(handleCached(http.FileServer(http.Dir("/static")))))
	mux.Handle("/data.js", gziphandler.GzipHandler(handleData(ja)))
	mux.Handle("/prowjobs.js", gziphandler.GzipHandler(handleProwJobs(ja)))
	mux.Handle("/log", gziphandler.GzipHandler(handleLog(ja)))
	mux.Handle("/rerun", gziphandler.GzipHandler(handleRerun(kc)))
	mux.Handle("/config", gziphandler.GzipHandler(handleConfig(configAgent)))
	mux.Handle("/branding.js", gziphandler.GzipHandler(handleBranding(configAgent)))

	if *hookURL != "" {
		mux.Handle("/plugin-help.js", gziphandler.GzipHandler(handlePluginHelp(newHelpAgent(*hookURL))))
	}

	if *tideURL != "" {
		ta := &tideAgent{
			log:          logger.WithField("agent", "tide"),
			path:         *tideURL,
			updatePeriod: func() time.Duration { return configAgent.Config().Deck.TideUpdatePeriod },
		}
		ta.start()
		mux.Handle("/tide.js", gziphandler.GzipHandler(handleTide(configAgent, ta)))
	}

	// optionally inject http->https redirect handler when behind loadbalancer
	if *redirectHTTPTo != "" {
		redirectMux := http.NewServeMux()
		redirectMux.Handle("/", func(oldMux *http.ServeMux, host string) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-forwarded-proto") == "http" {
					redirectURL, err := url.Parse(r.URL.String())
					if err != nil {
						logger.Errorf("Failed to parse URL: %s.", r.URL.String())
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
		}(mux, *redirectHTTPTo))
		mux = redirectMux
	}
	logger.WithError(http.ListenAndServe(":8080", mux)).Fatal("ListenAndServe returned.")
}

func loadToken(file string) (string, error) {
	raw, err := ioutil.ReadFile(file)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(raw)), nil
}

func handleCached(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This looks ridiculous but actually no-cache means "revalidate" and
		// "max-age=0" just means there is no time in which it can skip
		// revalidation. We also need to set must-revalidate because no-cache
		// doesn't imply must-revalidate when using the back button
		// https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.9.1
		// TODO(bentheelder): consider setting a longer max-age
		// setting it this way means the content is always revalidated
		w.Header().Set("Cache-Control", "public, max-age=0, no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

func setHeadersNoCaching(w http.ResponseWriter) {
	// Note that we need to set both no-cache and no-store because only some
	// broswers decided to (incorrectly) treat no-cache as "never store"
	// IE "no-store". for good measure to cover older browsers we also set
	// expires and pragma: https://stackoverflow.com/a/2068407
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func handleProwJobs(ja *JobAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		jobs := ja.ProwJobs()
		jd, err := json.Marshal(struct {
			Items []kube.ProwJob `json:"items"`
		}{jobs})
		if err != nil {
			logrus.WithError(err).Error("Error marshaling jobs.")
			jd = []byte("{}")
		}
		// If we have a "var" query, then write out "var value = {...};".
		// Otherwise, just write out the JSON.
		if v := r.URL.Query().Get("var"); v != "" {
			fmt.Fprintf(w, "var %s = %s;", v, string(jd))
		} else {
			fmt.Fprint(w, string(jd))
		}
	}
}

func handleData(ja *JobAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		jobs := ja.Jobs()
		jd, err := json.Marshal(jobs)
		if err != nil {
			logrus.WithError(err).Error("Error marshaling jobs.")
			jd = []byte("[]")
		}
		// If we have a "var" query, then write out "var value = {...};".
		// Otherwise, just write out the JSON.
		if v := r.URL.Query().Get("var"); v != "" {
			fmt.Fprintf(w, "var %s = %s;", v, string(jd))
		} else {
			fmt.Fprint(w, string(jd))
		}
	}
}

func handleTide(ca *config.Agent, ta *tideAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		queryConfigs := ca.Config().Tide.Queries
		queries := make([]string, 0, len(queryConfigs))
		for _, qc := range queryConfigs {
			queries = append(queries, qc.Query())
		}

		ta.Lock()
		defer ta.Unlock()
		payload := tideData{
			Queries:     queries,
			TideQueries: queryConfigs,
			Pools:       ta.pools,
		}
		pd, err := json.Marshal(payload)
		if err != nil {
			logrus.WithError(err).Error("Error marshaling payload.")
			pd = []byte("{}")
		}
		// If we have a "var" query, then write out "var value = {...};".
		// Otherwise, just write out the JSON.
		if v := r.URL.Query().Get("var"); v != "" {
			fmt.Fprintf(w, "var %s = %s;", v, string(pd))
		} else {
			fmt.Fprint(w, string(pd))
		}

	}
}

func handlePluginHelp(ha *helpAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		help, err := ha.getHelp()
		if err != nil {
			logrus.WithError(err).Error("Getting plugin help from hook.")
			help = &pluginhelp.Help{}
		}
		b, err := json.Marshal(*help)
		if err != nil {
			logrus.WithError(err).Error("Marshaling plugin help.")
			b = []byte("[]")
		}
		// If we have a "var" query, then write out "var value = [...];".
		// Otherwise, just write out the JSON.
		if v := r.URL.Query().Get("var"); v != "" {
			fmt.Fprintf(w, "var %s = %s;", v, string(b))
		} else {
			fmt.Fprint(w, string(b))
		}
	}
}

type logClient interface {
	GetJobLog(job, id string) ([]byte, error)
	// Add ability to stream logs with options enabled. This call is used to follow logs
	// using kubernetes client API. All other options on the Kubernetes log api can
	// also be enabled.
	GetJobLogStream(job, id string, options map[string]string) (io.ReadCloser, error)
}

func httpChunking(log io.ReadCloser, w http.ResponseWriter) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		logrus.Warning("Error getting flusher.")
	}
	reader := bufio.NewReader(log)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// TODO(rmmh): The log stops streaming after 30s.
			// This seems to be an apiserver limitation-- investigate?
			// logrus.WithError(err).Error("chunk failed to read!")
			break
		}
		w.Write(line)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func getOptions(values url.Values) map[string]string {
	options := make(map[string]string)
	for k, v := range values {
		if k != "pod" && k != "job" && k != "id" {
			options[k] = v[0]
		}
	}
	return options
}

// TODO(spxtr): Cache, rate limit.
func handleLog(lc logClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		job := r.URL.Query().Get("job")
		id := r.URL.Query().Get("id")
		stream := r.URL.Query().Get("follow")
		var logStreamRequested bool
		if ok, _ := strconv.ParseBool(stream); ok {
			// get http chunked responses to the client
			w.Header().Set("Connection", "Keep-Alive")
			w.Header().Set("Transfer-Encoding", "chunked")
			logStreamRequested = true
		}
		if err := validateLogRequest(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !logStreamRequested {
			log, err := lc.GetJobLog(job, id)
			if err != nil {
				http.Error(w, fmt.Sprintf("Log not found: %v", err), http.StatusNotFound)
				logrus.WithError(err).Warning("Error returned.")
				return
			}
			if _, err = w.Write(log); err != nil {
				logrus.WithError(err).Warning("Error writing log.")
			}
		} else {
			//run http chunking
			options := getOptions(r.URL.Query())
			log, err := lc.GetJobLogStream(job, id, options)
			if err != nil {
				http.Error(w, fmt.Sprintf("Log stream caused: %v", err), http.StatusNotFound)
				logrus.WithError(err).Warning("Error returned.")
				return
			}
			httpChunking(log, w)
		}
	}
}

func validateLogRequest(r *http.Request) error {
	job := r.URL.Query().Get("job")
	id := r.URL.Query().Get("id")

	if job == "" {
		return errors.New("Missing job query")
	}
	if id == "" {
		return errors.New("Missing ID query")
	}
	if !objReg.MatchString(job) {
		return fmt.Errorf("Invalid job query: %s", job)
	}
	if !objReg.MatchString(id) {
		return fmt.Errorf("Invalid ID query: %s", id)
	}
	return nil
}

type pjClient interface {
	GetProwJob(string) (kube.ProwJob, error)
}

func handleRerun(kc pjClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("prowjob")
		if !objReg.MatchString(name) {
			http.Error(w, "Invalid ProwJob query", http.StatusBadRequest)
			return
		}
		pj, err := kc.GetProwJob(name)
		if err != nil {
			http.Error(w, fmt.Sprintf("ProwJob not found: %v", err), http.StatusNotFound)
			logrus.WithError(err).Warning("Error returned.")
			return
		}
		pjutil := pjutil.NewProwJob(pj.Spec, pj.ObjectMeta.Labels)
		b, err := yaml.Marshal(&pjutil)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error marshaling: %v", err), http.StatusInternalServerError)
			logrus.WithError(err).Error("Error marshaling jobs.")
			return
		}
		if _, err := w.Write(b); err != nil {
			logrus.WithError(err).Error("Error writing log.")
		}
	}
}

func handleConfig(ca configAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// TODO(bentheelder): add the ability to query for portions of the config?
		setHeadersNoCaching(w)
		config := ca.Config()
		b, err := yaml.Marshal(config)
		if err != nil {
			logrus.WithError(err).Error("Error marshaling config.")
			http.Error(w, "Failed to marhshal config.", http.StatusInternalServerError)
			return
		}
		buff := bytes.NewBuffer(b)
		_, err = buff.WriteTo(w)
		if err != nil {
			logrus.WithError(err).Error("Error writing config.")
			http.Error(w, "Failed to write config.", http.StatusInternalServerError)
		}
	}
}

func handleBranding(ca configAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setHeadersNoCaching(w)
		config := ca.Config()
		b, err := json.Marshal(config.Deck.Branding)
		if err != nil {
			logrus.WithError(err).Error("Error marshaling branding config.")
			http.Error(w, "Failed to marhshal branding config.", http.StatusInternalServerError)
			return
		}
		// If we have a "var" query, then write out "var value = [...];".
		// Otherwise, just write out the JSON.
		if v := r.URL.Query().Get("var"); v != "" {
			fmt.Fprintf(w, "var %s = %s;", v, string(b))
		} else {
			fmt.Fprint(w, string(b))
		}
	}
}
