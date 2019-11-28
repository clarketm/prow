/*
Copyright The Kubernetes Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
	v1 "github.com/clarketm/prow/apis/prowjobs/v1"
	scheme "github.com/clarketm/prow/client/clientset/versioned/scheme"
)

// ProwJobsGetter has a method to return a ProwJobInterface.
// A group's client should implement this interface.
type ProwJobsGetter interface {
	ProwJobs(namespace string) ProwJobInterface
}

// ProwJobInterface has methods to work with ProwJob resources.
type ProwJobInterface interface {
	Create(*v1.ProwJob) (*v1.ProwJob, error)
	Update(*v1.ProwJob) (*v1.ProwJob, error)
	UpdateStatus(*v1.ProwJob) (*v1.ProwJob, error)
	Delete(name string, options *metav1.DeleteOptions) error
	DeleteCollection(options *metav1.DeleteOptions, listOptions metav1.ListOptions) error
	Get(name string, options metav1.GetOptions) (*v1.ProwJob, error)
	List(opts metav1.ListOptions) (*v1.ProwJobList, error)
	Watch(opts metav1.ListOptions) (watch.Interface, error)
	Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1.ProwJob, err error)
	ProwJobExpansion
}

// prowJobs implements ProwJobInterface
type prowJobs struct {
	client rest.Interface
	ns     string
}

// newProwJobs returns a ProwJobs
func newProwJobs(c *ProwV1Client, namespace string) *prowJobs {
	return &prowJobs{
		client: c.RESTClient(),
		ns:     namespace,
	}
}

// Get takes name of the prowJob, and returns the corresponding prowJob object, and an error if there is any.
func (c *prowJobs) Get(name string, options metav1.GetOptions) (result *v1.ProwJob, err error) {
	result = &v1.ProwJob{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("prowjobs").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do().
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of ProwJobs that match those selectors.
func (c *prowJobs) List(opts metav1.ListOptions) (result *v1.ProwJobList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1.ProwJobList{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("prowjobs").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do().
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested prowJobs.
func (c *prowJobs) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Namespace(c.ns).
		Resource("prowjobs").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch()
}

// Create takes the representation of a prowJob and creates it.  Returns the server's representation of the prowJob, and an error, if there is any.
func (c *prowJobs) Create(prowJob *v1.ProwJob) (result *v1.ProwJob, err error) {
	result = &v1.ProwJob{}
	err = c.client.Post().
		Namespace(c.ns).
		Resource("prowjobs").
		Body(prowJob).
		Do().
		Into(result)
	return
}

// Update takes the representation of a prowJob and updates it. Returns the server's representation of the prowJob, and an error, if there is any.
func (c *prowJobs) Update(prowJob *v1.ProwJob) (result *v1.ProwJob, err error) {
	result = &v1.ProwJob{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("prowjobs").
		Name(prowJob.Name).
		Body(prowJob).
		Do().
		Into(result)
	return
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().

func (c *prowJobs) UpdateStatus(prowJob *v1.ProwJob) (result *v1.ProwJob, err error) {
	result = &v1.ProwJob{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("prowjobs").
		Name(prowJob.Name).
		SubResource("status").
		Body(prowJob).
		Do().
		Into(result)
	return
}

// Delete takes name of the prowJob and deletes it. Returns an error if one occurs.
func (c *prowJobs) Delete(name string, options *metav1.DeleteOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("prowjobs").
		Name(name).
		Body(options).
		Do().
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *prowJobs) DeleteCollection(options *metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	var timeout time.Duration
	if listOptions.TimeoutSeconds != nil {
		timeout = time.Duration(*listOptions.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Namespace(c.ns).
		Resource("prowjobs").
		VersionedParams(&listOptions, scheme.ParameterCodec).
		Timeout(timeout).
		Body(options).
		Do().
		Error()
}

// Patch applies the patch and returns the patched prowJob.
func (c *prowJobs) Patch(name string, pt types.PatchType, data []byte, subresources ...string) (result *v1.ProwJob, err error) {
	result = &v1.ProwJob{}
	err = c.client.Patch(pt).
		Namespace(c.ns).
		Resource("prowjobs").
		SubResource(subresources...).
		Name(name).
		Body(data).
		Do().
		Into(result)
	return
}
