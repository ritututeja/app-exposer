package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/cyverse-de/app-exposer/apps"
	"github.com/gorilla/mux"
	"github.com/gosimple/slug"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"gopkg.in/cyverse-de/model.v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

var log = logrus.WithFields(logrus.Fields{
	"service": "app-exposer",
	"art-id":  "app-exposer",
	"group":   "org.cyverse",
})

func slugString(str string) string {
	slug.MaxLength = 63
	text := slug.Make(str)
	return strings.ReplaceAll(text, "_", "-")
}

// Init contains configuration for configuring an *Internal.
type Init struct {
	PorklockImage                 string
	PorklockTag                   string
	InputPathListIdentifier       string
	TicketInputPathListIdentifier string
	ViceProxyImage                string
	CASBaseURL                    string
	FrontendBaseURL               string
	ViceDefaultBackendService     string
	ViceDefaultBackendServicePort int
	GetAnalysisIDService          string
	CheckResourceAccessService    string
	VICEBackendNamespace          string
	AppsServiceBaseURL            string
	ViceNamespace                 string
	JobStatusURL                  string
}

// Internal contains information and operations for launching VICE apps inside the
// local k8s cluster.
type Internal struct {
	Init
	clientset       kubernetes.Interface
	db              *sql.DB
	statusPublisher AnalysisStatusPublisher
}

// New creates a new *Internal.
func New(init *Init, db *sql.DB, clientset kubernetes.Interface) *Internal {
	return &Internal{
		Init:      *init,
		db:        db,
		clientset: clientset,
		statusPublisher: &JSLPublisher{
			statusURL: init.JobStatusURL,
		},
	}
}

// labelsFromJob returns a map[string]string that can be used as labels for K8s resources.
func (i *Internal) labelsFromJob(job *model.Job) (map[string]string, error) {
	name := []rune(job.Name)

	var stringmax int
	if len(name) >= 63 {
		stringmax = 62
	} else {
		stringmax = len(name) - 1
	}

	a := apps.NewApps(i.db)
	ipAddr, err := a.GetUserIP(job.UserID)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"external-id":   job.InvocationID,
		"app-name":      slugString(job.AppName),
		"app-id":        job.AppID,
		"username":      slugString(job.Submitter),
		"user-id":       job.UserID,
		"analysis-name": slugString(string(name[:stringmax])),
		"app-type":      "interactive",
		"subdomain":     IngressName(job.UserID, job.InvocationID),
		"login-ip":      ipAddr,
	}, nil
}

// UpsertExcludesConfigMap uses the Job passed in to assemble the ConfigMap
// containing the files that should not be uploaded to iRODS. It then calls
// the k8s API to create the ConfigMap if it does not already exist or to
// update it if it does.
func (i *Internal) UpsertExcludesConfigMap(job *model.Job) error {
	excludesCM, err := i.excludesConfigMap(job)
	if err != nil {
		return err
	}

	cmclient := i.clientset.CoreV1().ConfigMaps(i.ViceNamespace)

	_, err = cmclient.Get(excludesConfigMapName(job), metav1.GetOptions{})
	if err != nil {
		log.Info(err)
		_, err = cmclient.Create(excludesCM)
		if err != nil {
			return err
		}
	} else {
		_, err = cmclient.Update(excludesCM)
		if err != nil {
			return err
		}
	}
	return nil
}

// UpsertInputPathListConfigMap uses the Job passed in to assemble the ConfigMap
// containing the path list of files to download from iRODS for the VICE analysis.
// It then uses the k8s API to create the ConfigMap if it does not already exist or to
// update it if it does.
func (i *Internal) UpsertInputPathListConfigMap(job *model.Job) error {
	inputCM, err := i.inputPathListConfigMap(job)
	if err != nil {
		return err
	}

	cmclient := i.clientset.CoreV1().ConfigMaps(i.ViceNamespace)

	_, err = cmclient.Get(inputPathListConfigMapName(job), metav1.GetOptions{})
	if err != nil {
		_, err = cmclient.Create(inputCM)
		if err != nil {
			return err
		}
	} else {
		_, err = cmclient.Update(inputCM)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpsertDeployment uses the Job passed in to assemble a Deployment for the
// VICE analysis. If then uses the k8s API to create the Deployment if it does
// not already exist or to update it if it does.
func (i *Internal) UpsertDeployment(job *model.Job) error {
	deployment, err := i.getDeployment(job)
	if err != nil {
		return err
	}

	depclient := i.clientset.AppsV1().Deployments(i.ViceNamespace)
	_, err = depclient.Get(job.InvocationID, metav1.GetOptions{})
	if err != nil {
		_, err = depclient.Create(deployment)
		if err != nil {
			return err
		}
	} else {
		_, err = depclient.Update(deployment)
		if err != nil {
			return err
		}
	}

	// Create the service for the job.
	svc, err := i.getService(job, deployment)
	if err != nil {
		return err
	}
	svcclient := i.clientset.CoreV1().Services(i.ViceNamespace)
	_, err = svcclient.Get(job.InvocationID, metav1.GetOptions{})
	if err != nil {
		_, err = svcclient.Create(svc)
		if err != nil {
			return err
		}
	}

	// Create the ingress for the job
	ingress, err := i.getIngress(job, svc)
	if err != nil {
		return err
	}

	ingressclient := i.clientset.ExtensionsV1beta1().Ingresses(i.ViceNamespace)
	_, err = ingressclient.Get(ingress.Name, metav1.GetOptions{})
	if err != nil {
		_, err = ingressclient.Create(ingress)
		if err != nil {
			return err
		}
	}

	return nil
}

// VICELaunchApp is the HTTP handler that orchestrates the launching of a VICE analysis inside
// the k8s cluster. This get passed to the router to be associated with a route. The Job
// is passed in as the body of the request.
func (i *Internal) VICELaunchApp(writer http.ResponseWriter, request *http.Request) {
	job := &model.Job{}

	buf, err := ioutil.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	if err = json.Unmarshal(buf, job); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	if err = i.validateJob(job); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	// Create the excludes file ConfigMap for the job.
	if err = i.UpsertExcludesConfigMap(job); err != nil {
		if err != nil {
			http.Error(
				writer,
				err.Error(),
				http.StatusInternalServerError,
			)
			return
		}
	}

	// Create the input path list config map
	if err = i.UpsertInputPathListConfigMap(job); err != nil {
		if err != nil {
			http.Error(
				writer,
				err.Error(),
				http.StatusInternalServerError,
			)
			return
		}
	}

	// Create the deployment for the job.
	if err = i.UpsertDeployment(job); err != nil {
		if err != nil {
			http.Error(
				writer,
				err.Error(),
				http.StatusInternalServerError,
			)
			return
		}
	}
}

// VICETriggerDownloads handles requests to trigger file downloads.
func (i *Internal) VICETriggerDownloads(writer http.ResponseWriter, request *http.Request) {
	var err error
	if err = i.doFileTransfer(request, downloadBasePath, downloadKind, true); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

// VICETriggerUploads handles requests to trigger file uploads.
func (i *Internal) VICETriggerUploads(writer http.ResponseWriter, request *http.Request) {
	var err error
	if err = i.doFileTransfer(request, uploadBasePath, uploadKind, true); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

// VICEExit terminates the VICE analysis deployment and cleans up
// resources asscociated with it. Does not save outputs first. Uses
// the external-id label to find all of the objects in the configured
// namespace associated with the job. Deletes the following objects:
// ingresses, services, deployments, and configmaps.
func (i *Internal) VICEExit(writer http.ResponseWriter, request *http.Request) {
	id := mux.Vars(request)["id"]

	set := labels.Set(map[string]string{
		"external-id": id,
	})

	listoptions := metav1.ListOptions{
		LabelSelector: set.AsSelector().String(),
	}

	// Delete the ingress
	ingressclient := i.clientset.ExtensionsV1beta1().Ingresses(i.ViceNamespace)
	ingresslist, err := ingressclient.List(listoptions)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, ingress := range ingresslist.Items {
		if err = ingressclient.Delete(ingress.Name, &metav1.DeleteOptions{}); err != nil {
			log.Error(err)
		}
	}

	// Delete the service
	svcclient := i.clientset.CoreV1().Services(i.ViceNamespace)
	svclist, err := svcclient.List(listoptions)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, svc := range svclist.Items {
		if err = svcclient.Delete(svc.Name, &metav1.DeleteOptions{}); err != nil {
			log.Error(err)
		}
	}

	// Delete the deployment
	depclient := i.clientset.AppsV1().Deployments(i.ViceNamespace)
	deplist, err := depclient.List(listoptions)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, dep := range deplist.Items {
		if err = depclient.Delete(dep.Name, &metav1.DeleteOptions{}); err != nil {
			log.Error(err)
		}
	}

	// Delete the input files list and the excludes list config maps
	cmclient := i.clientset.CoreV1().ConfigMaps(i.ViceNamespace)
	cmlist, err := cmclient.List(listoptions)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("number of configmaps to be deleted for %s: %d", id, len(cmlist.Items))

	for _, cm := range cmlist.Items {
		log.Infof("deleting configmap %s for %s", cm.Name, id)
		if err = cmclient.Delete(cm.Name, &metav1.DeleteOptions{}); err != nil {
			log.Error(err)
		}
	}
}

func (i *Internal) getIDFromHost(host string) (string, error) {
	ingressclient := i.clientset.ExtensionsV1beta1().Ingresses(i.ViceNamespace)
	ingresslist, err := ingressclient.List(metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	for _, ingress := range ingresslist.Items {
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == host {
				return ingress.Name, nil
			}
		}
	}

	return "", fmt.Errorf("no ingress found for host %s", host)
}

// VICEStatus handles requests to check the status of a running VICE app in K8s.
// This will return an overall status and status for the individual containers in
// the app's pod. Uses the state of the readiness checks in K8s, along with the
// existence of the various resources created for the app.
func (i *Internal) VICEStatus(writer http.ResponseWriter, request *http.Request) {
	var (
		ingressExists bool
		serviceExists bool
		podReady      bool
	)

	host := mux.Vars(request)["host"]

	id, err := i.getIDFromHost(host)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusNotFound)
		return
	}

	// If getIDFromHost returns without an error, then the ingress exists
	// since the ingresses are looked at for the host.
	ingressExists = true

	set := labels.Set(map[string]string{
		"external-id": id,
	})

	listoptions := metav1.ListOptions{
		LabelSelector: set.AsSelector().String(),
	}

	// check the service existence
	svcclient := i.clientset.CoreV1().Services(i.ViceNamespace)
	svclist, err := svcclient.List(listoptions)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(svclist.Items) > 0 {
		serviceExists = true
	}

	// Check pod status through the deployment
	depclient := i.clientset.AppsV1().Deployments(i.ViceNamespace)
	deplist, err := depclient.List(listoptions)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, dep := range deplist.Items {
		if dep.Status.ReadyReplicas > 0 {
			podReady = true
		}
	}

	data := map[string]bool{
		"ready": ingressExists && serviceExists && podReady,
	}

	body, err := json.Marshal(data)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(writer, string(body))
}

// VICESaveAndExit handles requests to save the output files in iRODS and then exit.
// The exit portion will only occur if the save operation succeeds. The operation is
// performed inside of a goroutine so that the caller isn't waiting for hours/days for
// output file transfers to complete.
func (i *Internal) VICESaveAndExit(writer http.ResponseWriter, request *http.Request) {
	log.Info("save and exit called")

	// Since file transfers can take a while, we should do this asynchronously by default.
	go func(writer http.ResponseWriter, request *http.Request) {
		var err error

		log.Info("calling doFileTransfer")

		// Trigger a blocking output file transfer request.
		if err = i.doFileTransfer(request, uploadBasePath, uploadKind, false); err != nil {
			log.Error(errors.Wrap(err, "error doing file transfer")) // Log but don't exit. Possible to cancel a job that hasn't started yet
		}

		log.Info("calling VICEExit")

		i.VICEExit(writer, request)

		log.Info("after VICEExit")
	}(writer, request)

	log.Info("leaving save and exit")
}

const updateTimeLimitSQL = `
	UPDATE ONLY jobs
	   SET planned_end_date = old_value.planned_end_date + interval '72 hours'
	  FROM (SELECT planned_end_date FROM jobs WHERE id = $2) AS old_value
	 WHERE jobs.id = $2
	   AND jobs.user_id = $1
 RETURNING jobs.planned_end_date
`

const getTimeLimitSQL = `
	SELECT planned_end_date
	  FROM jobs
	 WHERE jobs.id = $2
	   AND jobs.user_id = $1
`

const getUserIDSQL = `
	SELECT users.id
	  FROM users
	 WHERE username = $1
`

// VICETimeLimitUpdate handles requests to update the time limit on an already running VICE app.
func (i *Internal) VICETimeLimitUpdate(writer http.ResponseWriter, request *http.Request) {
	log.Info("update time limit called")

	var (
		err    error
		id     string
		users  []string
		user   string
		userID string
		found  bool
	)

	// user is required
	if users, found = request.URL.Query()["user"]; !found {
		http.Error(writer, "user is not set", http.StatusForbidden)
		return
	}
	user = users[0]

	if !strings.HasSuffix(user, "@iplantcollaborative.org") {
		user = fmt.Sprintf("%s@iplantcollaborative.org", user)
	}

	// id is required
	if id, found = mux.Vars(request)["analysis-id"]; !found {
		http.Error(writer, errors.New("id parameter is empty").Error(), http.StatusBadRequest)
		return
	}

	if err = i.db.QueryRow(getUserIDSQL, user).Scan(&userID); err != nil {
		http.Error(writer, errors.Wrapf(err, "error looking user ID for %s", user).Error(), http.StatusBadRequest)
		return
	}

	var newTimeLimit pq.NullTime
	if err = i.db.QueryRow(updateTimeLimitSQL, userID, id).Scan(&newTimeLimit); err != nil {
		http.Error(writer, errors.Wrapf(err, "error extending time limit for user %s on analysis %s", userID, id).Error(), http.StatusBadRequest)
		return
	}

	outputMap := map[string]string{}
	if newTimeLimit.Valid {
		v, err := newTimeLimit.Value()
		if err != nil {
			http.Error(writer, errors.Wrapf(err, "error getting new time limit for user %s on analysis %s", userID, id).Error(), http.StatusInternalServerError)
			return
		}
		outputMap["time_limit"] = fmt.Sprintf("%d", v.(time.Time).Unix())
	} else {
		http.Error(writer, errors.Wrapf(err, "the time limit for analysis %s was null after extension", id).Error(), http.StatusInternalServerError)
		return
	}

	var outputJSON []byte
	outputJSON, err = json.Marshal(outputMap)
	if err != nil {
		http.Error(writer, errors.Wrapf(err, "error marshalling the JSON for the new time limit for analysis %s", id).Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprint(writer, string(outputJSON))
}

// VICEGetTimeLimit implements the handler for getting the current time limit from the database.
func (i *Internal) VICEGetTimeLimit(writer http.ResponseWriter, request *http.Request) {
	log.Info("get time limit called")

	var (
		err    error
		id     string
		users  []string
		user   string
		userID string
		found  bool
	)

	// user is required
	if users, found = request.URL.Query()["user"]; !found {
		http.Error(writer, "user is not set", http.StatusForbidden)
		return
	}
	user = users[0]

	if !strings.HasSuffix(user, "@iplantcollaborative.org") {
		user = fmt.Sprintf("%s@iplantcollaborative.org", user)
	}

	// id is required
	if id, found = mux.Vars(request)["analysis-id"]; !found {
		http.Error(writer, errors.New("id parameter is empty").Error(), http.StatusBadRequest)
		return
	}

	if err = i.db.QueryRow(getUserIDSQL, user).Scan(&userID); err != nil {
		http.Error(writer, errors.Wrapf(err, "error looking user ID for %s", user).Error(), http.StatusBadRequest)
		return
	}

	var timeLimit pq.NullTime
	if err = i.db.QueryRow(getTimeLimitSQL, userID, id).Scan(&timeLimit); err != nil {
		http.Error(writer, errors.Wrapf(err, "error retrieving time limit for user %s on analysis %s", userID, id).Error(), http.StatusBadRequest)
		return
	}

	outputMap := map[string]string{}
	if timeLimit.Valid {
		v, err := timeLimit.Value()
		if err != nil {
			http.Error(writer, errors.Wrapf(err, "error getting time limit for user %s on analysis %s", userID, id).Error(), http.StatusInternalServerError)
			return
		}
		outputMap["time_limit"] = fmt.Sprintf("%d", v.(time.Time).Unix())
	} else {
		outputMap["time_limit"] = "null"
	}

	var outputJSON []byte
	outputJSON, err = json.Marshal(outputMap)
	if err != nil {
		http.Error(writer, errors.Wrapf(err, "error marshalling the JSON for the time limit for analysis %s", id).Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprint(writer, string(outputJSON))
}
