/*
Copyright 2018 The Kubernetes Authors.

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

package decorate

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"k8s.io/test-infra/prow/clonerefs"
	"k8s.io/test-infra/prow/entrypoint"
	"k8s.io/test-infra/prow/gcsupload"
	"k8s.io/test-infra/prow/initupload"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pod-utils/clone"
	"k8s.io/test-infra/prow/pod-utils/downwardapi"
	"k8s.io/test-infra/prow/pod-utils/wrapper"
	"k8s.io/test-infra/prow/sidecar"
)

const (
	logMountName            = "logs"
	logMountPath            = "/logs"
	artifactsEnv            = "ARTIFACTS"
	artifactsPath           = logMountPath + "/artifacts"
	codeMountName           = "code"
	codeMountPath           = "/home/prow/go"
	gopathEnv               = "GOPATH"
	toolsMountName          = "tools"
	toolsMountPath          = "/tools"
	gcsCredentialsMountName = "gcs-credentials"
	gcsCredentialsMountPath = "/secrets/gcs"
)

// Labels returns a string slice with label consts from kube.
func Labels() []string {
	return []string{kube.ProwJobTypeLabel, kube.CreatedByProw, kube.ProwJobIDLabel}
}

// VolumeMounts returns a string slice with *MountName consts in it.
func VolumeMounts() []string {
	return []string{logMountName, codeMountName, toolsMountName, gcsCredentialsMountName}
}

// VolumeMountPaths returns a string slice with *MountPath consts in it.
func VolumeMountPaths() []string {
	return []string{logMountPath, codeMountPath, toolsMountPath, gcsCredentialsMountPath}
}

// LabelsAndAnnotationsForSpec returns a minimal set of labels to add to prowjobs or its owned resources.
//
// User-provided extraLabels and extraAnnotations values will take precedence over auto-provided values.
func LabelsAndAnnotationsForSpec(spec kube.ProwJobSpec, extraLabels, extraAnnotations map[string]string) (map[string]string, map[string]string) {
	jobNameForLabel := spec.Job
	if len(jobNameForLabel) > validation.LabelValueMaxLength {
		// TODO(fejta): consider truncating middle rather than end.
		jobNameForLabel = strings.TrimRight(spec.Job[:validation.LabelValueMaxLength], ".-")
		logrus.WithFields(logrus.Fields{
			"job":       spec.Job,
			"key":       kube.ProwJobAnnotation,
			"value":     spec.Job,
			"truncated": jobNameForLabel,
		}).Info("Cannot use full job name, will truncate.")
	}
	labels := map[string]string{
		kube.CreatedByProw:     "true",
		kube.ProwJobTypeLabel:  string(spec.Type),
		kube.ProwJobAnnotation: jobNameForLabel,
	}
	if spec.Type != kube.PeriodicJob && spec.Refs != nil {
		labels[kube.OrgLabel] = spec.Refs.Org
		labels[kube.RepoLabel] = spec.Refs.Repo
		if len(spec.Refs.Pulls) > 0 {
			labels[kube.PullLabel] = strconv.Itoa(spec.Refs.Pulls[0].Number)
		}
	}

	for k, v := range extraLabels {
		labels[k] = v
	}

	// let's validate labels
	for key, value := range labels {
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			// try to use basename of a path, if path contains invalid //
			base := filepath.Base(value)
			if errs := validation.IsValidLabelValue(base); len(errs) == 0 {
				labels[key] = base
				continue
			}
			logrus.WithFields(logrus.Fields{
				"key":    key,
				"value":  value,
				"errors": errs,
			}).Warn("Removing invalid label")
			delete(labels, key)
		}
	}

	annotations := map[string]string{
		kube.ProwJobAnnotation: spec.Job,
	}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}

	return labels, annotations
}

// LabelsAndAnnotationsForJob returns a standard set of labels to add to pod/build/etc resources.
func LabelsAndAnnotationsForJob(pj kube.ProwJob) (map[string]string, map[string]string) {
	var extraLabels map[string]string
	if extraLabels = pj.ObjectMeta.Labels; extraLabels == nil {
		extraLabels = map[string]string{}
	}
	extraLabels[kube.ProwJobIDLabel] = pj.ObjectMeta.Name
	return LabelsAndAnnotationsForSpec(pj.Spec, extraLabels, nil)
}

// ProwJobToPod converts a ProwJob to a Pod that will run the tests.
func ProwJobToPod(pj kube.ProwJob, buildID string) (*v1.Pod, error) {
	if pj.Spec.PodSpec == nil {
		return nil, fmt.Errorf("prowjob %q lacks a pod spec", pj.Name)
	}

	rawEnv, err := downwardapi.EnvForSpec(downwardapi.NewJobSpec(pj.Spec, buildID, pj.Name))
	if err != nil {
		return nil, err
	}

	spec := pj.Spec.PodSpec.DeepCopy()
	spec.RestartPolicy = "Never"
	spec.Containers[0].Name = kube.TestContainerName

	// if the user has not provided a serviceaccount to use or explicitly
	// requested mounting the default token, we treat the unset value as
	// false, while kubernetes treats it as true if it is unset because
	// it was added in v1.6
	if spec.AutomountServiceAccountToken == nil && spec.ServiceAccountName == "" {
		myFalse := false
		spec.AutomountServiceAccountToken = &myFalse
	}

	if pj.Spec.DecorationConfig == nil {
		spec.Containers[0].Env = append(spec.Containers[0].Env, kubeEnv(rawEnv)...)
	} else {
		if err := decorate(spec, &pj, rawEnv); err != nil {
			return nil, fmt.Errorf("error decorating podspec: %v", err)
		}
	}

	podLabels, annotations := LabelsAndAnnotationsForJob(pj)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pj.ObjectMeta.Name,
			Labels:      podLabels,
			Annotations: annotations,
		},
		Spec: *spec,
	}, nil
}

const cloneLogPath = "clone.json"

// CloneLogPath returns the path to the clone log file in the volume mount.
func CloneLogPath(logMount kube.VolumeMount) string {
	return filepath.Join(logMount.MountPath, cloneLogPath)
}

// Exposed for testing
const (
	cloneRefsName    = "clonerefs"
	cloneRefsCommand = "/clonerefs"
)

// cloneEnv encodes clonerefs Options into json and puts it into an environment variable
func cloneEnv(opt clonerefs.Options) ([]v1.EnvVar, error) {
	// TODO(fejta): use flags
	cloneConfigEnv, err := clonerefs.Encode(opt)
	if err != nil {
		return nil, err
	}
	return kubeEnv(map[string]string{clonerefs.JSONConfigEnvVar: cloneConfigEnv}), nil
}

// sshVolume converts a secret holding ssh keys into the corresponding volume and mount.
//
// This is used by CloneRefs to attach the mount to the clonerefs container.
func sshVolume(secret string) (kube.Volume, kube.VolumeMount) {
	var sshKeyMode int32 = 0400 // this is octal, so symbolic ref is `u+r`
	name := strings.Join([]string{"ssh-keys", secret}, "-")
	mountPath := path.Join("/secrets/ssh", secret)
	v := kube.Volume{
		Name: name,
		VolumeSource: kube.VolumeSource{
			Secret: &kube.SecretSource{
				SecretName:  secret,
				DefaultMode: &sshKeyMode,
			},
		},
	}

	vm := kube.VolumeMount{
		Name:      name,
		MountPath: mountPath,
		ReadOnly:  true,
	}

	return v, vm
}

// cookiefileVolumes converts a secret holding cookies into the corresponding volume and mount.
//
// Secret can be of the form secret-name/base-name or just secret-name.
// Here secret-name refers to the kubernetes secret volume to mount, and base-name refers to the key in the secret
// where the cookies are stored. The secret-name pattern is equivalent to secret-name/secret-name.
//
// This is used by CloneRefs to attach the mount to the clonerefs container.
// The returned string value is the path to the cookiefile for use with --cookiefile.
func cookiefileVolume(secret string) (kube.Volume, kube.VolumeMount, string) {
	// Separate secret-name/key-in-secret
	parts := strings.SplitN(secret, "/", 2)
	cookieSecret := parts[0]
	var base string
	if len(parts) == 1 {
		base = parts[0] // Assume key-in-secret == secret-name
	} else {
		base = parts[1]
	}
	var cookiefileMode int32 = 0400 // u+r
	vol := kube.Volume{
		Name: "cookiefile",
		VolumeSource: kube.VolumeSource{
			Secret: &kube.SecretSource{
				SecretName:  cookieSecret,
				DefaultMode: &cookiefileMode,
			},
		},
	}
	mount := kube.VolumeMount{
		Name:      vol.Name,
		MountPath: "/secrets/cookiefile", // append base to flag
		ReadOnly:  true,
	}
	return vol, mount, path.Join(mount.MountPath, base)
}

// CloneRefs constructs the container and volumes necessary to clone the refs requested by the ProwJob.
//
// The container checks out repositories specified by the ProwJob Refs to `codeMount`.
// A log of what it checked out is written to `clone.json` in `logMount`.
//
// The container may need to mount SSH keys and/or cookiefiles in order to access private refs.
// CloneRefs returns a list of volumes containing these secrets required by the container.
func CloneRefs(pj kube.ProwJob, codeMount, logMount kube.VolumeMount) (*kube.Container, []kube.Refs, []kube.Volume, error) {
	if pj.Spec.DecorationConfig == nil {
		return nil, nil, nil, nil
	}
	if skip := pj.Spec.DecorationConfig.SkipCloning; skip != nil && *skip {
		return nil, nil, nil, nil
	}
	var cloneVolumes []kube.Volume
	var refs []kube.Refs // Do not return []*kube.Refs which we do not own
	if pj.Spec.Refs != nil {
		refs = append(refs, *pj.Spec.Refs)
	}
	for _, r := range pj.Spec.ExtraRefs {
		refs = append(refs, r)
	}
	if len(refs) == 0 { // nothing to clone
		return nil, nil, nil, nil
	}
	if codeMount.Name == "" || codeMount.MountPath == "" {
		return nil, nil, nil, fmt.Errorf("codeMount must set Name and MountPath")
	}
	if logMount.Name == "" || logMount.MountPath == "" {
		return nil, nil, nil, fmt.Errorf("logMount must set Name and MountPath")
	}

	var cloneMounts []kube.VolumeMount
	var sshKeyPaths []string
	for _, secret := range pj.Spec.DecorationConfig.SSHKeySecrets {
		volume, mount := sshVolume(secret)
		cloneMounts = append(cloneMounts, mount)
		sshKeyPaths = append(sshKeyPaths, mount.MountPath)
		cloneVolumes = append(cloneVolumes, volume)
	}

	var cloneArgs []string
	var cookiefilePath string

	if cp := pj.Spec.DecorationConfig.CookiefileSecret; cp != "" {
		v, vm, vp := cookiefileVolume(cp)
		cloneMounts = append(cloneMounts, vm)
		cloneVolumes = append(cloneVolumes, v)
		cookiefilePath = vp
		cloneArgs = append(cloneArgs, "--cookiefile="+cookiefilePath)
	}

	env, err := cloneEnv(clonerefs.Options{
		CookiePath:       cookiefilePath,
		GitRefs:          refs,
		GitUserEmail:     clonerefs.DefaultGitUserEmail,
		GitUserName:      clonerefs.DefaultGitUserName,
		HostFingerprints: pj.Spec.DecorationConfig.SSHHostFingerprints,
		KeyFiles:         sshKeyPaths,
		Log:              CloneLogPath(logMount),
		SrcRoot:          codeMount.MountPath,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("clone env: %v", err)
	}

	container := kube.Container{
		Name:         cloneRefsName,
		Image:        pj.Spec.DecorationConfig.UtilityImages.CloneRefs,
		Command:      []string{cloneRefsCommand},
		Args:         cloneArgs,
		Env:          env,
		VolumeMounts: append([]kube.VolumeMount{logMount, codeMount}, cloneMounts...),
	}
	return &container, refs, cloneVolumes, nil
}

func processLog(log kube.VolumeMount, prefix string) string {
	if prefix == "" {
		return filepath.Join(log.MountPath, "process-log.txt")
	}
	return filepath.Join(log.MountPath, fmt.Sprintf("%s-log.txt", prefix))
}

func markerFile(log kube.VolumeMount, prefix string) string {
	if prefix == "" {
		return filepath.Join(log.MountPath, "marker-file.txt")
	}
	return filepath.Join(log.MountPath, fmt.Sprintf("%s-marker.txt", prefix))
}

func metadataFile(log kube.VolumeMount, prefix string) string {
	ad := artifactsDir(log)
	if prefix == "" {
		return filepath.Join(ad, "metadata.json")
	}
	return filepath.Join(ad, fmt.Sprintf("%s-metadata.json", prefix))
}

func artifactsDir(log kube.VolumeMount) string {
	return filepath.Join(log.MountPath, "artifacts")
}

func entrypointLocation(tools kube.VolumeMount) string {
	return filepath.Join(tools.MountPath, "entrypoint")
}

// InjectEntrypoint will make the entrypoint binary in the tools volume the container's entrypoint, which will output to the log volume.
func InjectEntrypoint(c *kube.Container, timeout, gracePeriod time.Duration, prefix, previousMarker string, exitZero bool, log, tools kube.VolumeMount) (*wrapper.Options, error) {
	wrapperOptions := &wrapper.Options{
		Args:         append(c.Command, c.Args...),
		ProcessLog:   processLog(log, prefix),
		MarkerFile:   markerFile(log, prefix),
		MetadataFile: metadataFile(log, prefix),
	}
	// TODO(fejta): use flags
	entrypointConfigEnv, err := entrypoint.Encode(entrypoint.Options{
		ArtifactDir:    artifactsDir(log),
		GracePeriod:    gracePeriod,
		Options:        wrapperOptions,
		Timeout:        timeout,
		AlwaysZero:     exitZero,
		PreviousMarker: previousMarker,
	})
	if err != nil {
		return nil, err
	}

	c.Command = []string{entrypointLocation(tools)}
	c.Args = nil
	c.Env = append(c.Env, kubeEnv(map[string]string{entrypoint.JSONConfigEnvVar: entrypointConfigEnv})...)
	c.VolumeMounts = append(c.VolumeMounts, log, tools)
	return wrapperOptions, nil
}

// PlaceEntrypoint will copy entrypoint from the entrypoint image to the tools volume
func PlaceEntrypoint(image string, toolsMount kube.VolumeMount) kube.Container {
	return kube.Container{
		Name:         "place-entrypoint",
		Image:        image,
		Command:      []string{"/bin/cp"},
		Args:         []string{"/entrypoint", entrypointLocation(toolsMount)},
		VolumeMounts: []kube.VolumeMount{toolsMount},
	}
}

func GCSOptions(dc kube.DecorationConfig) (kube.Volume, kube.VolumeMount, gcsupload.Options) {
	vol := kube.Volume{
		Name: gcsCredentialsMountName,
		VolumeSource: kube.VolumeSource{
			Secret: &kube.SecretSource{
				SecretName: dc.GCSCredentialsSecret,
			},
		},
	}
	mount := kube.VolumeMount{
		Name:      vol.Name,
		MountPath: gcsCredentialsMountPath,
	}
	opt := gcsupload.Options{
		// TODO: pass the artifact dir here too once we figure that out
		GCSConfiguration:   dc.GCSConfiguration,
		GcsCredentialsFile: fmt.Sprintf("%s/service-account.json", mount.MountPath),
		DryRun:             false,
	}

	return vol, mount, opt
}

func InitUpload(image string, opt gcsupload.Options, creds kube.VolumeMount, cloneLogMount *kube.VolumeMount, encodedJobSpec string) (*kube.Container, error) {
	// TODO(fejta): remove encodedJobSpec
	initUploadOptions := initupload.Options{
		Options: &opt,
	}
	var mounts []kube.VolumeMount
	if cloneLogMount != nil {
		initUploadOptions.Log = CloneLogPath(*cloneLogMount)
		mounts = append(mounts, *cloneLogMount)
	}
	mounts = append(mounts, creds)
	// TODO(fejta): use flags
	initUploadConfigEnv, err := initupload.Encode(initUploadOptions)
	if err != nil {
		return nil, fmt.Errorf("could not encode initupload configuration as JSON: %v", err)
	}
	return &kube.Container{
		Name:    "initupload",
		Image:   image,
		Command: []string{"/initupload"}, // TODO(fejta): remove this, use image's entrypoint and delete /initupload symlink
		Env: kubeEnv(map[string]string{
			downwardapi.JobSpecEnv:      encodedJobSpec,
			initupload.JSONConfigEnvVar: initUploadConfigEnv,
		}),
		VolumeMounts: mounts,
	}, nil
}

func decorate(spec *kube.PodSpec, pj *kube.ProwJob, rawEnv map[string]string) error {
	// TODO(fejta): we should pass around volume names rather than forcing particular mount paths.

	rawEnv[artifactsEnv] = artifactsPath
	rawEnv[gopathEnv] = codeMountPath // TODO(fejta): remove this once we can assume go modules
	logMount := kube.VolumeMount{
		Name:      logMountName,
		MountPath: logMountPath,
	}
	logVolume := kube.Volume{
		Name: logMountName,
		VolumeSource: kube.VolumeSource{
			EmptyDir: &kube.EmptyDirVolumeSource{},
		},
	}

	codeMount := kube.VolumeMount{
		Name:      codeMountName,
		MountPath: codeMountPath,
	}
	codeVolume := kube.Volume{
		Name: codeMountName,
		VolumeSource: kube.VolumeSource{
			EmptyDir: &kube.EmptyDirVolumeSource{},
		},
	}

	toolsMount := kube.VolumeMount{
		Name:      toolsMountName,
		MountPath: toolsMountPath,
	}
	toolsVolume := kube.Volume{
		Name: toolsMountName,
		VolumeSource: kube.VolumeSource{
			EmptyDir: &kube.EmptyDirVolumeSource{},
		},
	}

	gcsVol, gcsMount, gcsOptions := GCSOptions(*pj.Spec.DecorationConfig)

	cloner, refs, cloneVolumes, err := CloneRefs(*pj, codeMount, logMount)
	if err != nil {
		return fmt.Errorf("create clonerefs container: %v", err)
	}
	var cloneLogMount *kube.VolumeMount
	if cloner != nil {
		spec.InitContainers = append([]kube.Container{*cloner}, spec.InitContainers...)
		cloneLogMount = &logMount
	}

	encodedJobSpec := rawEnv[downwardapi.JobSpecEnv]
	initUpload, err := InitUpload(pj.Spec.DecorationConfig.UtilityImages.InitUpload, gcsOptions, gcsMount, cloneLogMount, encodedJobSpec)
	if err != nil {
		return fmt.Errorf("create initupload container: %v", err)
	}
	spec.InitContainers = append(
		spec.InitContainers,
		*initUpload,
		PlaceEntrypoint(pj.Spec.DecorationConfig.UtilityImages.Entrypoint, toolsMount),
	)

	spec.Containers[0].Env = append(spec.Containers[0].Env, kubeEnv(rawEnv)...)

	const ( // these values may change when/if we support multiple containers
		prefix   = "" // unique per container
		previous = ""
		exitZero = false
	)
	wrapperOptions, err := InjectEntrypoint(&spec.Containers[0], pj.Spec.DecorationConfig.Timeout, pj.Spec.DecorationConfig.GracePeriod, prefix, previous, exitZero, logMount, toolsMount)
	if err != nil {
		return fmt.Errorf("wrap container: %v", err)
	}

	sidecar, err := Sidecar(pj.Spec.DecorationConfig.UtilityImages.Sidecar, gcsOptions, gcsMount, logMount, encodedJobSpec, *wrapperOptions)
	if err != nil {
		return fmt.Errorf("create sidecar: %v", err)
	}

	spec.Containers = append(spec.Containers, *sidecar)
	spec.Volumes = append(spec.Volumes, logVolume, toolsVolume, gcsVol)

	if len(refs) > 0 {
		spec.Containers[0].WorkingDir = clone.PathForRefs(codeMount.MountPath, refs[0])
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, codeMount)
		spec.Volumes = append(spec.Volumes, append(cloneVolumes, codeVolume)...)
	}

	return nil
}

func Sidecar(image string, gcsOptions gcsupload.Options, gcsMount, logMount kube.VolumeMount, encodedJobSpec string, wrappers ...wrapper.Options) (*kube.Container, error) {
	gcsOptions.Items = append(gcsOptions.Items, artifactsDir(logMount))
	sidecarConfigEnv, err := sidecar.Encode(sidecar.Options{
		GcsOptions: &gcsOptions,
		Entries:    wrappers,
	})
	if err != nil {
		return nil, err
	}

	return &kube.Container{
		Name:    "sidecar",
		Image:   image,
		Command: []string{"/sidecar"}, // TODO(fejta): remove, use image's entrypoint
		Env: kubeEnv(map[string]string{
			sidecar.JSONConfigEnvVar: sidecarConfigEnv,
			downwardapi.JobSpecEnv:   encodedJobSpec, // TODO: shouldn't need this?
		}),
		VolumeMounts: []kube.VolumeMount{logMount, gcsMount},
	}, nil

}

// kubeEnv transforms a mapping of environment variables
// into their serialized form for a PodSpec, sorting by
// the name of the env vars
func kubeEnv(environment map[string]string) []v1.EnvVar {
	var keys []string
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var kubeEnvironment []v1.EnvVar
	for _, key := range keys {
		kubeEnvironment = append(kubeEnvironment, v1.EnvVar{
			Name:  key,
			Value: environment[key],
		})
	}

	return kubeEnvironment
}
