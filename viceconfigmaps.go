package main

import (
	"fmt"
	jobtmpl "github.com/cyverse-de/job-templates"
	"gopkg.in/cyverse-de/model.v4"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// excludesConfigMapName returns the name of the ConfigMap containing the list
// of paths that should be excluded from file uploads to iRODS by porklock.
func excludesConfigMapName(job *model.Job) string {
	return fmt.Sprintf("excludes-file-%s", job.InvocationID)
}

// excludesConfigMap returns the ConfigMap containing the list of paths
// that should be excluded from file uploads to iRODS by porklock. This does NOT
// call the k8s API to actually create the ConfigMap, just returns the object
// that can be passed to the API.
func excludesConfigMap(job *model.Job) apiv1.ConfigMap {
	labels := labelsFromJob(job)

	return apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   excludesConfigMapName(job),
			Labels: labels,
		},
		Data: map[string]string{
			excludesFileName: jobtmpl.ExcludesFileContents(job).String(),
		},
	}
}

// inputPathListConfigMapName returns the name of the ConfigMap containing
// the list of paths that should be downloaded from iRODS by porklock
// as input files for the VICE analysis.
func inputPathListConfigMapName(job *model.Job) string {
	return fmt.Sprintf("input-path-list-%s", job.InvocationID)
}

// inputPathListConfigMap returns the ConfigMap object containing the the
// list of paths that should be downloaded from iRODS by porklock as input
// files for the VICE analysis. This does NOT call the k8s API to actually
// create the ConfigMap, just returns the object that can be passed to the API.
func (e *ExposerApp) inputPathListConfigMap(job *model.Job) (*apiv1.ConfigMap, error) {
	labels := labelsFromJob(job)

	fileContents, err := jobtmpl.InputPathListContents(job, e.InputPathListIdentifier, e.TicketInputPathListIdentifier)
	if err != nil {
		return nil, err
	}

	return &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   inputPathListConfigMapName(job),
			Labels: labels,
		},
		Data: map[string]string{
			inputPathListFileName: fileContents.String(),
		},
	}, nil
}
