// Package external is a kuberhealthy checker that acts as an operator
// to run external images as checks.
package external

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
	log "github.com/sirupsen/logrus"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// DefaultKuberhealthyReportingURL is the default location that external checks
// are expected to report into.
const DefaultKuberhealthyReportingURL = "http://kuberhealthy.kuberhealthy.svc.local"

// kuberhealthyRunIDLabel is the pod label for the kuberhealthy run id value
const kuberhealthyRunIDLabel = "kuberhealthy-run-id"

// kuberhealthyCheckNameLabel is the label used to flag this pod as being managed by this checker
const kuberhealthyCheckNameLabel = "kuberhealthy-check-name"

// defaultMaxRunTime is the default time a pod is allowed to run when this checker is created
const defaultMaxRunTime = time.Minute * 15

// defaultMaxStartTime is the default time that a pod is required to start within
const defaultMaxStartTime = time.Minute * 5

// NamePrefix is the name of this kuberhealthy checker
var NamePrefix = DefaultName

// DefaultName is used when no check name is supplied
var DefaultName = "external-check"

// namespace indicates the namespace of the kuberhealthy
// pod that is running this check
var namespace = os.Getenv("POD_NAMESPACE")

// defaultRunInterval is the default time we assume this check
// should run on unless specified
var defaultRunInterval = time.Minute * 10

// Checker implements a KuberhealthyCheck for external
// check execution and lifecycle management.
type Checker struct {
	CheckName                string // the name of this checker
	Namespace                string
	ErrorMessages            []string
	RunInterval              time.Duration // how often this check runs a loop
	maxRunTime               time.Duration // time check must run completely within after switching to 'Running'
	startupTimeout           time.Duration // the time an external checker pod has to become 'Running' after starting
	kubeClient               *kubernetes.Clientset
	PodSpec                  *apiv1.PodSpec // the user-provided spec of the pod
	PodDeployed              bool           // indicates the pod exists in the API
	PodDeployedMu            sync.Mutex
	PodName                  string // the name of the deployed pod
	RunID                    string // the uuid of the current run
	KuberhealthyReportingURL string // the URL that the check should want to report results back to
	currentCheckUUID         string // the UUID of the current external checker running
	Debug bool // indicates we should run in debug mode - run once and stop
}

// New creates a new external checker
func New() (*Checker, error) {

	testChecker := Checker{
		ErrorMessages:            []string{},
		Namespace:                namespace,
		CheckName:                DefaultName,
		RunInterval:              defaultRunInterval,
		KuberhealthyReportingURL: DefaultKuberhealthyReportingURL,
		maxRunTime:               defaultMaxRunTime,
		startupTimeout:           defaultMaxStartTime,
		PodName: 				  DefaultName,
	}

	return &testChecker, nil
}

// CurrentStatus returns the status of the check as of right now
func (ext *Checker) CurrentStatus() (bool, []string) {
	if len(ext.ErrorMessages) > 0 {
		return false, ext.ErrorMessages
	}
	return true, ext.ErrorMessages
}

// Name returns the name of this check.  This name is used
// when creating a check status CRD as well as for the status
// output
func (ext *Checker) Name() string {
	return NamePrefix + "-" + ext.CheckName
}

// CheckNamespace returns the namespace of this checker
func (ext *Checker) CheckNamespace() string {
	return ext.Namespace
}

// Interval returns the interval at which this check runs
func (ext *Checker) Interval() time.Duration {
	return ext.RunInterval
}

// Timeout returns the maximum run time for this check before it times out
func (ext *Checker) Timeout() time.Duration {
	return ext.maxRunTime
}

// Run executes the checker.  This is ran on each "tick" of
// the RunInterval and is executed by the Kuberhealthy checker
func (ext *Checker) Run(client *kubernetes.Clientset) error {

	// run on a loop firing off external checks on each run
	for {
		// skip the initial pause if in debug mode
		if !ext.Debug {
			time.Sleep(ext.Interval())
		}

		// run a check iteration
		ext.log("Running external check iteration")
		err :=  ext.execute(client)
		if err != nil {
			ext.log("Error with running external check:", err.Error())
			ext.setError(err.Error())
		}

		// only run once if in debug mode
		if ext.Debug {
			return nil
		}
	}

}

func (ext *Checker) execute(client *kubernetes.Clientset) error {
	// set the clientset property with the passed client
	ext.kubeClient = client

	// generate a new UUID for this run:
	err := ext.createCheckUUID()
	if err != nil {
		return err
	}

	// validate the pod spec
	ext.log("Validating pod spec of external check")
	err = ext.validatePodSpec()
	if err != nil {
		return err
	}

	// condition the spec with the required labels and environment variables
	ext.log("Configuring spec of external check")
	err = ext.configureUserPodSpec()
	if err != nil {
		return errors.New("failed to configure pod spec for Kubernetes from user specified pod spec: " + err.Error())
	}

	// sanity check our settings
	ext.log("Running sanity check on check parameters")
	err = ext.sanityCheck()
	if err != nil {
		return err
	}

	// cleanup all pods from this checker that should not exist right now (all of them)
	ext.log("Deleting any rogue check pods")
	err = ext.deletePod()
	if err != nil {
		return errors.New("failed to clean up pods before starting external checker: " + err.Error())
	}

	// Spawn kubernetes pod to run our external check
	ext.log("creating pod for external check:", ext.CheckName)
	createdPod, err := ext.createPod()
	if err != nil {
		return errors.New("failed to create pod for checker: " + err.Error())
	}
	ext.log("Check", ext.Name(), "created pod", createdPod.Name, "in namespace", createdPod.Namespace)

	// wait for the pod to be running or timeout
	// make a cancel context so we can reign in the pod watch when its time
	ctx, cancelPodWatch := context.WithCancel(context.Background())

	// watch for pod to start with a timeout (include time for a new node to be created)
	select {
	case <-time.After(ext.startupTimeout):
		cancelPodWatch() // cancel the watch context, we have timed out
		err := ext.deletePod()
		errorMessage := "failed to see pod running within timeout"
		if err != nil {
			errorMessage = errorMessage + " and an error occurred when deleting the pod:" + err.Error()
		}
		return errors.New(errorMessage)
	case <-ext.waitForPodRunning(ctx):
		ext.log("External check pod is running:", ext.PodName)
	}

	// flag the pod as running
	ext.setPodDeployed(true)

	// the pod has started! Wait for the pod to exit and abort if it takes too long
	select {
	case <-time.After(ext.maxRunTime):
		cancelPodWatch() // cancel the watch context, we have timed out
		err := ext.deletePod()
		errorMessage := "pod ran too long and was shut down"
		if err != nil {
			errorMessage = errorMessage + " but an error occurred when deleting the pod:" + err.Error()
		}
		return errors.New(errorMessage)
	case <-ext.waitForPodExit(ctx):
		ext.log("External check pod is done running:", ext.PodName)
	}

	return nil
}

// Log writes a normal InfoLn message output prefixed with this checker's name on it
func (ext *Checker) log(s ...string) {
	log.Infoln(ext.CheckName+":", s)
}

// stopPod stops any pods running because of this external checker
func (ext *Checker) deletePod() error {
	ext.log("Deleting all checker pods")
	podClient := ext.kubeClient.CoreV1().Pods(ext.Namespace)
	return podClient.DeleteCollection(&metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: kuberhealthyCheckNameLabel + "=" + ext.CheckName,
	})
}


// sanityCheck runs a basic sanity check on the checker before running
func (ext *Checker) sanityCheck() error {
	if ext.Namespace == "" {
		return errors.New("check namespace can not be empty")
	}

	if ext.PodName == "" {
		return errors.New("pod name can not be empty")
	}

	if ext.kubeClient == nil {
		return errors.New("kubeClient can not be nil")
	}

	return nil
}

// waitForPodExit returns a channel that notifies when the checker pod exits
func (ext *Checker) waitForPodExit(ctx context.Context) chan error {

	// make the output channel we will return
	outChan := make(chan error, 2)

	// setup a pod watching client for our current KH pod
	podClient := ext.kubeClient.CoreV1().Pods(ext.Namespace)
	watcher, err := podClient.Watch(metav1.ListOptions{
		LabelSelector: kuberhealthyRunIDLabel + "=" + ext.currentCheckUUID,
	})

	// return the watch error as a channel if found
	if err != nil {
		outChan <- err
		return outChan
	}

	// watch events and return when the pod is in state running
	for e := range watcher.ResultChan() {

		// try to cast the incoming object to a pod and skip the event if we cant
		p, ok := e.Object.(*apiv1.Pod)
		if !ok {
			continue
		}

		// make sure the pod coming through the event channel has the right check uuid label
		if p.Labels[kuberhealthyRunIDLabel] != ext.currentCheckUUID {
			continue
		}

		// read the status of this pod (its ours) and return if its succeeded or failed
		if p.Status.Phase == apiv1.PodSucceeded || p.Status.Phase == apiv1.PodFailed {
			outChan <- nil
			return outChan
		}

		// if the context is done, we break the loop and return
		select {
		case <-ctx.Done():
			outChan <- errors.New("external checker pod completion watch aborted")
			return outChan
		default:
			// context is not canceled yet, continue
		}
	}

	outChan <- errors.New("external checker watch aborted pre-maturely")
	return outChan
}

// waitForPodRunning returns a channel that notifies when the checker pod is running
func (ext *Checker) waitForPodRunning(ctx context.Context) chan error {

	// make the output channel we will return
	outChan := make(chan error, 2)

	// setup a pod watching client for our current KH pod
	podClient := ext.kubeClient.CoreV1().Pods(ext.Namespace)
	watcher, err := podClient.Watch(metav1.ListOptions{
		LabelSelector: "kuberhealthy-run-id=" + ext.currentCheckUUID,
	})

	// return the watch error as a channel if found
	if err != nil {
		outChan <- err
		return outChan
	}

	// watch events and return when the pod is in state running
	for e := range watcher.ResultChan() {

		log.Debugln("Got event while watching for pod to start running:",e)

		// try to cast the incoming object to a pod and skip the event if we cant
		p, ok := e.Object.(*apiv1.Pod)
		if !ok {
			continue
		}

		// make sure the pod coming through the event channel has the right check uuid label
		if p.Labels[kuberhealthyRunIDLabel] != ext.currentCheckUUID {
			continue
		}

		// read the status of this pod (its ours)
		if p.Status.Phase == apiv1.PodRunning || p.Status.Phase == apiv1.PodFailed {
			outChan <- nil
			return outChan
		}

		// if the context is done, we break the loop and return
		select {
		case <-ctx.Done():
			outChan <- errors.New("external checker pod startup watch aborted")
			return outChan
		default:
			// context is not canceled yet, continue
		}
	}

	outChan <- errors.New("external checker watch aborted pre-maturely")
	return outChan
}

// validatePodSpec validates the user specified pod spec to ensure it looks like it
// has all the default configuration required
func (ext *Checker) validatePodSpec() error {

	// if the pod spec is unspecified, we return an error
	if ext.PodSpec == nil {
		return errors.New("unable to determine pod spec for checker  Pod spec was nil")
	}

	// if containers are not set, then we return an error
	if len(ext.PodSpec.Containers) == 0 && len(ext.PodSpec.InitContainers) == 0 {
		return errors.New("no containers found in checks PodSpec")
	}

	// ensure that at least one container is defined
	if len(ext.PodSpec.Containers) == 0 {
		return errors.New("no containers found in checks PodSpec")
	}

	// ensure that all containers have an image set
	for _, c := range ext.PodSpec.Containers {
		if len(c.Image) == 0 {
			return errors.New("no image found in check's PodSpec for container " + c.Name + ".")
		}
	}

	return nil
}

// createPod prepares and creates the checker pod using the kubernetes API
func (ext *Checker) createPod() (*apiv1.Pod, error) {
	p := &apiv1.Pod{}
	p.Namespace = ext.Namespace
	p.Name = ext.PodName
	ext.log("Creating external checker pod named", p.Name)
	p.Spec = *ext.PodSpec
	ext.addKuberhealthyLabels(p)
	return ext.kubeClient.CoreV1().Pods(ext.Namespace).Create(p)
}

// configureUserPodSpec configures a user-specified pod spec with
// the unique and required fields for compatibility with an external
// kuberhealthy check.

// configureUserPodSpec takes in the user's pod spec as seen by this checker and
// adds in kuberhealthy settings such as the required environment variables.
func (ext *Checker) configureUserPodSpec() error {

	// set overrides like env var for pod name and env var for where to send responses to
	// Set environment variable for run UUID
	// wrap pod spec in job spec

	// set the pod running the check's pod name
	ext.PodSpec.Hostname = ext.CheckName

	// specify environment variables that need applied.  We apply environment
	// variables that set the report-in URL of kuberhealthy along with
	// the unique run ID of this pod
	overwriteEnvVars := []apiv1.EnvVar{
		{
			Name:  "KUBERHEALTHY_URL",
			Value: ext.KuberhealthyReportingURL,
		},
		{
			Name:  "KUBERHEALTHY_RUN_ID",
			Value: ext.currentCheckUUID,
		},
	}

	// apply overwrite env vars on every container in the pod
	for i := range ext.PodSpec.Containers {
		ext.PodSpec.Containers[i].Env = append(ext.PodSpec.Containers[i].Env, overwriteEnvVars...)
	}

	// enforce restart policy of never
	ext.PodSpec.RestartPolicy = apiv1.RestartPolicyNever

	return nil
}

// addKuberhealthyLabels adds the appropriate labels to a kuberhealthy
// external checker pod.
func (ext *Checker) addKuberhealthyLabels(pod *apiv1.Pod) {
	// make the labels map if it does not exist on the pod yet
	if pod == nil {
		pod = &apiv1.Pod{}
	}
	if pod.ObjectMeta.Labels == nil {
		pod.ObjectMeta.Labels = make(map[string]string)
	}

	// stack the kuberhealthy run id on top of the existing labels
	existingLabels := pod.ObjectMeta.Labels
	existingLabels[kuberhealthyRunIDLabel] = ext.currentCheckUUID
	existingLabels[kuberhealthyCheckNameLabel] = ext.CheckName
}

// createCheckUUID creates a UUID that represents a single run of the external check
func (ext *Checker) createCheckUUID() error {
	uniqueID := uuid.New()
	ext.currentCheckUUID = uniqueID.String()
	return nil
}

// podExsts fetches the pod for the checker from the api server
// and returns a bool indicating if it exists or not
func (ext *Checker) podExists() (bool, error) {

	// setup a pod watching client for our current KH pod
	podClient := ext.kubeClient.CoreV1().Pods(ext.Namespace)
	p, err := podClient.Get(ext.PodName,metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	// if the pod has a start time that isn't zero, it exists
	if !p.Status.StartTime.IsZero() && p.Status.Phase != apiv1.PodFailed{
		return true, nil
	}

	return false, nil

}

// waitForShutdown waits for the external pod to shut down
func (ext *Checker) waitForShutdown(ctx context.Context) error {
	// repeatedly fetch the pod until its gone or the context
	// is canceled
	for {
		time.Sleep(time.Second / 2)
		exists, err := ext.podExists()
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}

		// see if the context has expired yet and give up if so
		select {
			case <-ctx.Done():
				return errors.New("timed out when waiting for pod to shutdown")
		default:
		}
	}
}

// Shutdown signals the checker to begin a shutdown and cleanup
func (ext *Checker) Shutdown() error {

	// make a context to satisfy pod removal
	ctx := context.Background()
	ctx, cancelCtx := context.WithCancel(ctx)

	// cancel the shutdown context after the timeout
	go func() {
		<-time.After(ext.Timeout())
		cancelCtx()
	}()

	// if the pod is deployed, delete it
	if ext.podDeployed() {
		err := ext.deletePod()
		if err != nil {
			ext.log("Error deleting pod during shutdown:", err.Error())
			return err
		}
		err = ext.waitForShutdown(ctx)
		if err != nil {
			ext.log("Error waiting for pod removal during shutdown:", err.Error())
			return err
		}
	}

	ext.log(ext.Name(), "Pod "+ext.PodName+" ready for shutdown.")
	return nil

}

// clearErrors clears all errors from the checker
func (ext *Checker) clearErrors() {
	ext.ErrorMessages = []string{}
}

// setError sets the error message for the checker and overwrites all prior state
func (ext *Checker) setError(s string) {
	ext.ErrorMessages = []string{s}
}

// addError adds an error to the errors list
func (ext *Checker) addError(s string) {
	ext.ErrorMessages = append(ext.ErrorMessages,s)
}

// podDeployed returns a bool indicating that the pod
// for this check exists and is deployed
func (ext *Checker) podDeployed() bool {
	ext.PodDeployedMu.Lock()
	defer ext.PodDeployedMu.Unlock()
	return ext.PodDeployed
}

// setPodDeployed sets the pod deployed state
func (ext *Checker) setPodDeployed(status bool) {
	ext.PodDeployedMu.Lock()
	defer ext.PodDeployedMu.Unlock()
	ext.PodDeployed = status
}

// getPodClient returns a client for Kubernetes pods
func (ext *Checker) getPodClient() typedv1.PodInterface {
	return ext.kubeClient.CoreV1().Pods(ext.Namespace)
}