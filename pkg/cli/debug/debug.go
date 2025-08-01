package debug

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	kappsv1 "k8s.io/api/apps/v1"
	kappsv1beta1 "k8s.io/api/apps/v1beta1"
	kappsv1beta2 "k8s.io/api/apps/v1beta2"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/kubectl/pkg/cmd/attach"
	"k8s.io/kubectl/pkg/cmd/logs"
	krun "k8s.io/kubectl/pkg/cmd/run"
	kcmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/interrupt"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/pod-security-admission/api"

	appsv1 "github.com/openshift/api/apps/v1"
	dockerv10 "github.com/openshift/api/image/docker10"
	imagev1 "github.com/openshift/api/image/v1"
	securityv1 "github.com/openshift/api/security/v1"
	appsv1client "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	imagev1client "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	"github.com/openshift/library-go/pkg/apps/appsutil"
	"github.com/openshift/library-go/pkg/image/imageutil"
	"github.com/openshift/library-go/pkg/image/reference"

	ocmdhelpers "github.com/openshift/oc/pkg/helpers/cmd"
	"github.com/openshift/oc/pkg/helpers/conditions"
	utilenv "github.com/openshift/oc/pkg/helpers/env"
	generateapp "github.com/openshift/oc/pkg/helpers/newapp/app"
)

const (
	debugPodAnnotationSourceContainer = "debug.openshift.io/source-container"
	debugPodAnnotationSourceResource  = "debug.openshift.io/source-resource"
	// containerResourcesAnnotationPrefix contains resource annotation prefix that will be used by CRI-O to set cpu shares
	containerResourcesAnnotationPrefix = "resources.workload.openshift.io/"
	// podWorkloadTargetAnnotationPrefix contains the prefix for the pod workload target annotation
	podWorkloadTargetAnnotationPrefix = "target.workload.openshift.io/"
	commandLinuxShell                 = "/bin/sh"
	commandWindowsShell               = "cmd.exe"
)

var (
	debugLong = templates.LongDesc(`
		Launch a command shell to debug a running application.

		When debugging images and setup problems, it's useful to get an exact copy of a running
		pod configuration and troubleshoot with a shell. Since a pod that is failing may not be
		started and not accessible to 'rsh' or 'exec', the 'debug' command makes it easy to
		create a carbon copy of that setup.

		The default mode is to start a shell inside of the first container of the referenced pod.
		The started pod will be a copy of your source pod, with labels stripped, the command
		changed to '/bin/sh' for Linux containers or 'cmd.exe' for Windows containers,
		and readiness and liveness checks disabled. If you just want to run
		a command, add '--' and a command to run. Passing a command will not create a TTY or send
		STDIN by default. Other flags are supported for altering the container or pod in common ways.

		A common problem running containers is a security policy that prohibits you from running
		as a root user on the cluster. You can use this command to test running a pod as
		non-root (with --as-user) or to run a non-root pod as root (with --as-root).

		You may invoke other types of objects besides pods - any controller resource that creates
		a pod (like a deployment, build, or job), objects that can host pods (like nodes), or
		resources that can be used to create pods (such as image stream tags), or simply pass
		'--image=IMAGE' to start a simple shell session in an image with a shell program

		The debug pod is deleted when the remote command completes or the user interrupts
		the shell.
	`)

	debugExample = templates.Examples(`
		# Start a shell session into a pod using the OpenShift tools image
		oc debug

		# Debug a currently running deployment by creating a new pod
		oc debug deploy/test

		# Debug a node as an administrator
		oc debug node/master-1

		# Debug a Windows node
		# Note: the chosen image must match the Windows Server version (2019, 2022) of the node
		oc debug node/win-worker-1 --image=mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022

		# Launch a shell in a pod using the provided image stream tag
		oc debug istag/mysql:latest -n openshift

		# Test running a job as a non-root user
		oc debug job/test --as-user=1000000

		# Debug a specific failing container by running the env command in the 'second' container
		oc debug daemonset/test -c second -- /bin/env

		# See the pod that would be created to debug
		oc debug mypod-9xbc -o yaml

		# Debug a resource but launch the debug pod in another namespace
		# Note: Not all resources can be debugged using --to-namespace without modification. For example,
		# volumes and service accounts are namespace-dependent. Add '-o yaml' to output the debug pod definition
		# to disk.  If necessary, edit the definition then run 'oc debug -f -' or run without --to-namespace
		oc debug mypod-9xbc --to-namespace testns
	`)
)

type DebugOptions struct {
	PrintFlags *genericclioptions.PrintFlags

	Attach attach.AttachOptions

	CoreClient  corev1client.CoreV1Interface
	AppsClient  appsv1client.AppsV1Interface
	ImageClient imagev1client.ImageV1Interface

	Printer          printers.ResourcePrinter
	LogsForObject    polymorphichelpers.LogsForObjectFunc
	RESTClientGetter genericclioptions.RESTClientGetter

	PreservePod bool
	NoStdin     bool
	TTY         bool
	DisableTTY  bool
	Timeout     time.Duration
	Quiet       bool

	Command            []string
	Annotations        map[string]string
	AsRoot             bool
	AsNonRoot          bool
	AsUser             int64
	KeepLabels         bool
	KeepAnnotations    bool
	KeepLiveness       bool
	KeepReadiness      bool
	KeepStartup        bool
	KeepInitContainers bool
	OneContainer       bool
	ContainerName      string
	NodeName           string
	NodeNameSet        bool
	AddEnv             []corev1.EnvVar
	RemoveEnv          []string
	Resources          []string
	Builder            func() *resource.Builder
	Namespace          string
	ExplicitNamespace  bool
	DryRun             bool
	FullCmdName        string
	Image              string
	ImageStream        string
	ToNamespace        string

	// IsNode is set after we see the object we're debugging.  We use it to be able to print pertinent advice.
	IsNode bool

	resource.FilenameOptions
	genericiooptions.IOStreams
}

func NewDebugOptions(streams genericiooptions.IOStreams) *DebugOptions {
	attachOpts := attach.NewAttachOptions(streams)
	attachOpts.TTY = true
	attachOpts.Stdin = true
	return &DebugOptions{
		PrintFlags:         genericclioptions.NewPrintFlags("").WithTypeSetter(scheme.Scheme),
		IOStreams:          streams,
		Timeout:            15 * time.Minute,
		KeepInitContainers: true,
		AsUser:             -1,
		Attach:             *attachOpts,
		LogsForObject:      polymorphichelpers.LogsForObjectFn,
	}
}

// NewCmdDebug creates a command for debugging pods.
func NewCmdDebug(f kcmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	o := NewDebugOptions(streams)
	cmd := &cobra.Command{
		Use:     "debug RESOURCE/NAME [ENV1=VAL1 ...] [-c CONTAINER] [flags] [-- COMMAND]",
		Short:   "Launch a new instance of a pod for debugging",
		Long:    debugLong,
		Example: debugExample,
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(cmd, f, args))
			kcmdutil.CheckErr(o.Validate())

			cmdErr := o.RunDebug()
			if o.IsNode {
				ocmdhelpers.CheckPodSecurityErr(cmdErr)
			} else {
				kcmdutil.CheckErr(cmdErr)
			}

		},
	}

	addDebugFlags(cmd, o)

	return cmd
}

func addDebugFlags(cmd *cobra.Command, o *DebugOptions) {
	usage := "to read a template"
	kcmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, usage)

	// FIXME-REBASE: we need to wire jsonpath here and other printers
	cmd.Flags().Bool("no-headers", false, "If true, when using the default output, don't print headers.")
	cmd.Flags().MarkHidden("no-headers")
	cmd.Flags().String("sort-by", "", "If non-empty, sort list types using this field specification.  The field specification is expressed as a JSONPath expression (e.g. 'ObjectMeta.Name'). The field in the API resource specified by this JSONPath expression must be an integer or a string.")
	cmd.Flags().MarkHidden("sort-by")
	cmd.Flags().Bool("show-all", true, "When printing, show all resources (default hide terminated pods.)")
	cmd.Flags().MarkHidden("show-all")
	cmd.Flags().Bool("show-labels", false, "When printing, show all labels as the last column (default hide labels column)")

	cmd.Flags().BoolVarP(&o.Quiet, "quiet", "q", o.Quiet, "No informational messages will be printed.")
	cmd.Flags().BoolVarP(&o.NoStdin, "no-stdin", "I", o.NoStdin, "Bypasses passing STDIN to the container, defaults to true if no command specified")
	cmd.Flags().BoolVarP(&o.TTY, "tty", "t", o.TTY, "Force a pseudo-terminal to be allocated")
	cmd.Flags().BoolVarP(&o.DisableTTY, "no-tty", "T", o.DisableTTY, "Disable pseudo-terminal allocation")
	cmd.Flags().StringVarP(&o.ContainerName, "container", "c", o.ContainerName, "Container name; defaults to first container")
	cmd.Flags().BoolVar(&o.KeepAnnotations, "keep-annotations", o.KeepAnnotations, "If true, keep the original pod annotations")
	cmd.Flags().BoolVar(&o.KeepLabels, "keep-labels", o.KeepLabels, "If true, keep the original pod labels")
	cmd.Flags().BoolVar(&o.KeepLiveness, "keep-liveness", o.KeepLiveness, "If true, keep the original pod liveness probes")
	cmd.Flags().BoolVar(&o.KeepInitContainers, "keep-init-containers", o.KeepInitContainers, "Run the init containers for the pod. Defaults to true.")
	cmd.Flags().BoolVar(&o.KeepReadiness, "keep-readiness", o.KeepReadiness, "If true, keep the original pod readiness probes")
	cmd.Flags().BoolVar(&o.KeepStartup, "keep-startup", o.KeepStartup, "If true, keep the original startup probes")
	cmd.Flags().BoolVar(&o.OneContainer, "one-container", o.OneContainer, "If true, run only the selected container, remove all others")
	cmd.Flags().StringVar(&o.NodeName, "node-name", o.NodeName, "Set a specific node to run on - by default the pod will run on any valid node")
	cmd.Flags().BoolVar(&o.AsRoot, "as-root", o.AsRoot, "If true, try to run the container as the root user")
	cmd.Flags().Int64Var(&o.AsUser, "as-user", o.AsUser, "Try to run the container as a specific user UID (note: admins may limit your ability to use this flag)")
	cmd.Flags().StringVar(&o.Image, "image", o.Image, "Override the image used by the targeted container.")
	cmd.Flags().StringVar(&o.ImageStream, "image-stream", o.ImageStream, "Specify an image stream (namespace/name:tag) containing a debug image to run.")
	cmd.Flags().StringVar(&o.ToNamespace, "to-namespace", o.ToNamespace, "Override the namespace to create the pod into (instead of using --namespace).")
	cmd.Flags().BoolVar(&o.PreservePod, "preserve-pod", o.PreservePod, "If true, the pod will not be deleted after the debug command exits.")

	o.PrintFlags.AddFlags(cmd)
	kcmdutil.AddDryRunFlag(cmd)
}

func (o *DebugOptions) Complete(cmd *cobra.Command, f kcmdutil.Factory, args []string) error {
	if i := cmd.ArgsLenAtDash(); i != -1 && i < len(args) {
		o.Command = args[i:]
		args = args[:i]
	}
	resources, envArgs, ok := utilenv.SplitEnvironmentFromResources(args)
	if !ok {
		return kcmdutil.UsageErrorf(cmd, "all resources must be specified before environment changes: %s", strings.Join(args, " "))
	}
	o.Resources = resources
	o.RESTClientGetter = f

	strategy, err := kcmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}
	o.DryRun = strategy != kcmdutil.DryRunNone

	switch {
	case o.TTY && o.NoStdin:
		return kcmdutil.UsageErrorf(cmd, "you may not specify -I and -t together")
	case o.TTY && o.DisableTTY:
		return kcmdutil.UsageErrorf(cmd, "you may not specify -t and -T together")
	case o.TTY:
		o.Attach.TTY = true
	// since ForceTTY is defaulted to false, check if user specifically passed in "=false" flag
	case !o.TTY && cmd.Flags().Changed("tty"):
		o.Attach.TTY = false
	case o.DisableTTY:
		o.Attach.TTY = false
	// don't default TTY to true if a command is passed
	case len(o.Command) > 0:
		o.Attach.TTY = false
		o.Attach.Stdin = false
	default:
		o.Attach.TTY = printers.IsTerminal(o.In)
		klog.V(4).Infof("Defaulting TTY to %t", o.Attach.TTY)
	}
	if o.NoStdin {
		o.Attach.TTY = false
		o.Attach.Stdin = false
	}

	o.Attach.Quiet = o.Quiet

	o.NodeNameSet = cmd.Flags().Changed("node-name")

	if o.Annotations == nil {
		o.Annotations = make(map[string]string)
	}

	if len(o.Command) == 0 {
		o.Command = []string{commandLinuxShell}
	}

	o.Namespace, o.ExplicitNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	o.Builder = f.NewBuilder

	o.AddEnv, o.RemoveEnv, err = utilenv.ParseEnv(envArgs, nil)
	if err != nil {
		return err
	}

	cmdParent := cmd.Parent()
	if cmdParent != nil && len(cmdParent.CommandPath()) > 0 && kcmdutil.IsSiblingCommandExists(cmd, "describe") {
		o.FullCmdName = cmdParent.CommandPath()
	}
	o.AsNonRoot = !o.AsRoot && cmd.Flag("as-root").Changed

	if o.PrintFlags.OutputFlagSpecified() {
		kcmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, strategy)
		o.Printer, err = o.PrintFlags.ToPrinter()
		if err != nil {
			return err
		}
	}

	config, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.Attach.Config = config

	o.CoreClient, err = corev1client.NewForConfig(config)
	if err != nil {
		return err
	}

	o.AppsClient, err = appsv1client.NewForConfig(config)
	if err != nil {
		return err
	}

	o.ImageClient, err = imagev1client.NewForConfig(config)
	if err != nil {
		return err
	}

	return nil
}

func (o DebugOptions) Validate() error {
	if (o.AsRoot || o.AsNonRoot) && o.AsUser > 0 {
		return fmt.Errorf("you may not specify --as-root and --as-user=%d at the same time", o.AsUser)
	}
	return nil
}

// Debug creates and runs a debugging pod.
func (o *DebugOptions) RunDebug() error {
	var infos []*resource.Info

	// the simplest possible debug is an image
	if len(o.Resources) == 0 && len(o.FilenameOptions.Filenames) == 0 {
		image := o.Image

		if len(image) == 0 {
			imageStream := o.ImageStream
			if len(imageStream) == 0 {
				imageStream = "openshift/tools:latest"
			}
			imageFromStream, err := o.resolveImageStreamTagString(imageStream)
			if err != nil {
				return fmt.Errorf("unable to resolve a default pod image from image stream %s: %v", imageStream, err)
			}
			image = imageFromStream
			klog.V(4).Infof("Defaulted image from imagestream %s: %s", imageStream, image)
		}

		infos = append(infos, &resource.Info{
			Mapping: &meta.RESTMapping{
				Resource:         corev1.SchemeGroupVersion.WithResource("pods"),
				GroupVersionKind: corev1.SchemeGroupVersion.WithKind("Pod"),
			},
			Name: "image",
			Object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "image",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "debug", Image: image},
					},
				},
			},
		})

	} else {
		b := o.Builder().
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			NamespaceParam(o.Namespace).DefaultNamespace().
			SingleResourceType().
			ResourceNames("pods", o.Resources...).
			Flatten()
		if len(o.FilenameOptions.Filenames) > 0 {
			b.FilenameParam(o.ExplicitNamespace, &o.FilenameOptions)
		}
		var err error
		infos, err = b.Do().Infos()
		if err != nil {
			return err
		}
	}
	if len(infos) != 1 {
		klog.V(4).Infof("Objects: %#v", infos)
		return fmt.Errorf("you must identify a single resource with a pod template to debug")
	}

	template, err := o.approximatePodTemplateForObject(infos[0].Object)
	if err != nil && template == nil {
		return fmt.Errorf("cannot debug %s: %v", infos[0].Name, err)
	}
	if err != nil {
		klog.V(4).Infof("Unable to get exact template, but continuing with fallback: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: template.ObjectMeta,
		Spec:       template.Spec,
	}

	ns, cleanup, err := o.getNamespace(infos[0].Namespace)
	if err != nil {
		return fmt.Errorf("unable to get namespace %v", err)
	}
	defer cleanup()

	pod.Name, pod.Namespace = fmt.Sprintf("%s-debug-%s", generateapp.MakeSimpleName(infos[0].Name), utilrand.String(5)), ns
	o.Attach.Pod = pod

	if len(o.ContainerName) == 0 && len(pod.Spec.Containers) > 0 {
		if !o.Quiet {
			if len(pod.Spec.Containers) > 1 && len(o.FullCmdName) > 0 {
				fmt.Fprintf(o.ErrOut, "Defaulting container name to %s.\n", pod.Spec.Containers[0].Name)
				fmt.Fprintf(o.ErrOut, "Use '%s describe pod/%s -n %s' to see all of the containers in this pod.\n", o.FullCmdName, pod.Name, pod.Namespace)
				fmt.Fprintf(o.ErrOut, "\n")
			}
		}

		klog.V(4).Infof("Defaulting container name to %s", pod.Spec.Containers[0].Name)
		o.ContainerName = pod.Spec.Containers[0].Name
	}

	names := containerNames(o.Attach.Pod)
	if len(names) == 0 {
		return fmt.Errorf("the provided pod must have at least one container")
	}
	if len(o.ContainerName) == 0 {
		return fmt.Errorf("you must provide a container name to debug")
	}
	if containerForName(o.Attach.Pod, o.ContainerName) == nil {
		return fmt.Errorf("the container %q is not a valid container name; must be one of %v", o.ContainerName, names)
	}

	o.Annotations[debugPodAnnotationSourceResource] = fmt.Sprintf("%s/%s", infos[0].Mapping.Resource, infos[0].Name)
	o.Annotations[debugPodAnnotationSourceContainer] = o.ContainerName

	if infos[0].Mapping.GroupVersionKind.Kind == "Node" {
		o.Annotations[securityv1.RequiredSCCAnnotation] = "privileged"
	}

	pod, originalCommand := o.transformPodForDebug(o.Annotations)
	var commandString string
	switch {
	case len(originalCommand) > 0:
		commandString = strings.Join(originalCommand, " ")
	default:
		commandString = ""
	}

	if o.Printer != nil {
		return o.Printer.PrintObj(pod, o.Out)
	}

	if o.DryRun {
		return nil
	}

	klog.V(5).Infof("Creating pod: %#v", pod)
	pod, err = o.createPod(pod)
	if err != nil {
		return err
	}

	// ensure the pod is cleaned up on shutdown
	o.Attach.InterruptParent = interrupt.New(
		func(os.Signal) { os.Exit(1) },
		func() {
			if o.PreservePod {
				return
			}
			stderr := o.ErrOut
			if stderr == nil {
				stderr = os.Stderr
			}
			if !o.Quiet {
				fmt.Fprintf(stderr, "\nRemoving debug pod ...\n")
			}
			if err := o.CoreClient.Pods(pod.Namespace).Delete(context.TODO(), pod.Name, *metav1.NewDeleteOptions(0)); err != nil {
				if !kapierrors.IsNotFound(err) {
					klog.V(2).Infof("Unable to delete the debug pod %q: %v", pod.Name, err)
					if !o.Quiet {
						fmt.Fprintf(stderr, "error: unable to delete the debug pod %q: %v\n", pod.Name, err)
					}
				}
			}
		},
	)

	klog.V(5).Infof("Created attach arguments: %#v", o.Attach)
	return o.Attach.InterruptParent.Run(func() error {
		if !o.Quiet {
			if len(commandString) > 0 {
				fmt.Fprintf(o.ErrOut, "Starting pod/%s, command was: %s\n", pod.Name, commandString)
			} else {
				fmt.Fprintf(o.ErrOut, "Starting pod/%s ...\n", pod.Name)
			}
			if o.IsNode {
				if !(template.Spec.OS != nil && template.Spec.OS.Name == corev1.Windows) {
					fmt.Fprintf(o.ErrOut, "To use host binaries, run `chroot /host`. Instead, if you need to access host namespaces, run `nsenter -a -t 1`.\n")
				}
			}
		}

		fieldSelector := fields.OneTermEqualSelector("metadata.name", pod.Name).String()
		lw := &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = fieldSelector
				return o.CoreClient.Pods(ns).List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = fieldSelector
				return o.CoreClient.Pods(ns).Watch(context.TODO(), options)
			},
		}
		preconditionFunc := func(store cache.Store) (bool, error) {
			_, exists, err := store.Get(&metav1.ObjectMeta{Namespace: ns, Name: pod.Name})
			if err != nil {
				return true, err
			}
			if !exists {
				// We need to make sure we see the object in the cache before we start waiting for events
				// or we would be waiting for the timeout if such object didn't exist.
				// (e.g. it was deleted before we started informers so they wouldn't even see the delete event)
				return true, kapierrors.NewNotFound(corev1.Resource("pods"), pod.Name)
			}

			return false, nil
		}

		notifyFn := func(pod *corev1.Pod, container corev1.ContainerStatus) error {
			// TODO: instead of reporting to the user a message, accumulate a certain amount of time in
			// the error state, then exit early
			if o.Quiet {
				return nil
			}
			if container.State.Waiting != nil {
				switch container.State.Waiting.Reason {
				case "CreateContainerError", "ImagePullBackOff":
					fmt.Fprintf(o.Attach.ErrOut, "warning: Container %s is unable to start due to an error: %s\n", container.Name, container.State.Waiting.Message)
				}
			}
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		containerRunningEvent, err := watchtools.UntilWithSync(ctx, lw, &corev1.Pod{}, preconditionFunc, conditions.PodContainerRunning(o.ContainerName, o.CoreClient, notifyFn))
		if err == nil {
			klog.V(4).Infof("Stopped waiting for pod: %s %#v", containerRunningEvent.Type, containerRunningEvent.Object)
		} else {
			klog.V(4).Infof("Stopped waiting for pod: %v", err)
		}

		switch {
		// api didn't error right away but the pod wasn't even created
		case kapierrors.IsNotFound(err):
			msg := fmt.Sprintf("unable to create the debug pod %q", pod.Name)
			if len(o.NodeName) > 0 {
				msg += fmt.Sprintf(" on node %q", o.NodeName)
			}
			return errors.New(msg)
			// switch to logging output
		case err == krun.ErrPodCompleted, err == conditions.ErrContainerTerminated:
			resultPod, ok := containerRunningEvent.Object.(*corev1.Pod)
			if ok {
				if resultPod.Status.Reason == "NodeAffinity" && len(resultPod.Spec.NodeSelector) != 0 {
					return fmt.Errorf("debug pod could not be scheduled: %v. To fix this you may want to create a new namespace with empty node selector and run the debug there. For example: oc adm new-project --node-selector=\"\" debug", resultPod.Status.Message)
				}
				for _, c := range resultPod.Status.Conditions {
					if c.Type == corev1.DisruptionTarget {
						msg := fmt.Sprintf("unable to create the debug pod %q", pod.Name)
						if len(o.NodeName) > 0 {
							msg += fmt.Sprintf(" on node %q", o.NodeName)
						}
						return errors.New(msg)
					}
				}
			}
			return o.getLogs(pod)
		case err == conditions.ErrNonZeroExitCode:
			if err = o.getLogs(pod); err != nil {
				return err
			}
			return conditions.ErrNonZeroExitCode
		case err != nil:
			return err
		case !o.Attach.Stdin:
			if err = o.getLogs(pod); err != nil {
				return err
			}
			lastWatchEvent, err := watchtools.UntilWithSync(ctx, lw, &corev1.Pod{}, preconditionFunc, conditions.PodDone)
			if err != nil {
				if kapierrors.IsNotFound(err) {
					return nil
				}
				return err
			}

			resultPod, ok := lastWatchEvent.Object.(*corev1.Pod)
			if ok {
				for _, s := range append(append([]corev1.ContainerStatus{}, resultPod.Status.InitContainerStatuses...), resultPod.Status.ContainerStatuses...) {
					if s.Name != o.ContainerName {
						continue
					}
					if s.State.Terminated != nil && s.State.Terminated.ExitCode != 0 {
						return conditions.ErrNonZeroExitCode
					}
				}
			}
			return nil
		default:
			if !o.Quiet {
				// TODO this doesn't do us much good for remote debugging sessions, but until we get a local port
				// set up to proxy, this is what we've got.
				if podWithStatus, ok := containerRunningEvent.Object.(*corev1.Pod); ok {
					fmt.Fprintf(o.Attach.ErrOut, "Pod IP: %s\n", podWithStatus.Status.PodIP)
				}
			}

			// TODO: attach can race with pod completion, allow attach to switch to logs
			o.Attach.ContainerName = o.ContainerName
			return o.Attach.Run()
		}
	})
}

// getContainerImageViaDeploymentConfig attempts to return an Image for a given
// Container.  It tries to walk from the Container's Pod to its DeploymentConfig
// (via the "openshift.io/deployment-config.name" annotation), then tries to
// find the ImageStream from which the DeploymentConfig is deploying, then tries
// to find a match for the Container's image in the ImageStream's Images.
func (o *DebugOptions) getContainerImageViaDeploymentConfig(pod *corev1.Pod, container *corev1.Container) (*imagev1.Image, error) {
	ref, err := reference.Parse(container.Image)
	if err != nil {
		return nil, err
	}

	if ref.ID == "" {
		return nil, nil // ID is needed for later lookup
	}

	dcname := pod.Annotations[appsv1.DeploymentConfigAnnotation]
	if dcname == "" {
		return nil, nil // Pod doesn't appear to have been created by a DeploymentConfig
	}

	dc, err := o.AppsClient.DeploymentConfigs(o.Attach.Pod.Namespace).Get(context.TODO(), dcname, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	for _, trigger := range dc.Spec.Triggers {
		if trigger.Type == appsv1.DeploymentTriggerOnImageChange &&
			trigger.ImageChangeParams != nil &&
			trigger.ImageChangeParams.From.Kind == "ImageStreamTag" {

			isname, _, err := imageutil.ParseImageStreamTagName(trigger.ImageChangeParams.From.Name)
			if err != nil {
				return nil, err
			}

			namespace := trigger.ImageChangeParams.From.Namespace
			if len(namespace) == 0 {
				namespace = o.Attach.Pod.Namespace
			}

			isi, err := o.ImageClient.ImageStreamImages(namespace).Get(context.TODO(), imageutil.JoinImageStreamImage(isname, ref.ID), metav1.GetOptions{})
			if err != nil {
				return nil, err
			}

			return &isi.Image, nil
		}
	}

	return nil, nil // DeploymentConfig doesn't have an ImageChange Trigger
}

// getContainerImageViaImageStreamImport attempts to return an Image for a given
// Container.  It does this by submiting a ImageStreamImport request with Import
// set to false.  The request will not succeed if the backing repository
// requires Insecure to be set to true, which cannot be hard-coded for security
// reasons.
func (o *DebugOptions) getContainerImageViaImageStreamImport(container *corev1.Container) (*imagev1.Image, error) {
	isi := &imagev1.ImageStreamImport{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oc-debug",
		},
		Spec: imagev1.ImageStreamImportSpec{
			Images: []imagev1.ImageImportSpec{
				{
					From: corev1.ObjectReference{
						Kind: "DockerImage",
						Name: container.Image,
					},
				},
			},
		},
	}

	isi, err := o.ImageClient.ImageStreamImports(o.Attach.Pod.Namespace).Create(context.TODO(), isi, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	if len(isi.Status.Images) > 0 {
		return isi.Status.Images[0].Image, nil
	}

	return nil, nil
}

func (o *DebugOptions) getContainerImageCommand(pod *corev1.Pod, container *corev1.Container) ([]string, error) {
	if len(container.Command) > 0 {
		return container.Command, nil
	}
	image, err := o.getContainerImageViaDeploymentConfig(pod, container)
	if err != nil {
		image, err = o.getContainerImageViaImageStreamImport(container)
		if err != nil {
			return nil, err
		}
	}

	if image == nil {
		return nil, fmt.Errorf("error: no usable image found")
	}

	if err := imageutil.ImageWithMetadata(image); err != nil {
		return nil, err
	}
	dockerImage, ok := image.DockerImageMetadata.Object.(*dockerv10.DockerImage)
	if !ok {
		return nil, err
	}

	return append(dockerImage.Config.Entrypoint, dockerImage.Config.Cmd...), nil
}

// transformPodForDebug alters the input pod to be debuggable
func (o *DebugOptions) transformPodForDebug(annotations map[string]string) (*corev1.Pod, []string) {
	pod := o.Attach.Pod

	// reset the container
	container := containerForName(pod, o.ContainerName)

	// identify the command to be run
	originalCommand, _ := o.getContainerImageCommand(pod, container)
	if len(container.Command) > 0 {
		originalCommand = container.Command
		originalCommand = append(originalCommand, container.Args...)
	}

	if len(o.Image) > 0 {
		container.Image = o.Image
	}

	command := o.getContainerCommand()
	container.Command = command
	container.Args = nil
	container.TTY = o.Attach.Stdin && o.Attach.TTY
	container.Stdin = o.Attach.Stdin
	container.StdinOnce = o.Attach.Stdin

	if !o.KeepReadiness {
		container.ReadinessProbe = nil
	}
	if !o.KeepLiveness {
		container.LivenessProbe = nil
	}
	if !o.KeepStartup {
		container.StartupProbe = nil
	}

	var newEnv []corev1.EnvVar
	if len(o.RemoveEnv) > 0 {
		for i := range container.Env {
			skip := false
			for _, name := range o.RemoveEnv {
				if name == container.Env[i].Name {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			newEnv = append(newEnv, container.Env[i])
		}
	} else {
		newEnv = container.Env
	}
	newEnv = append(newEnv, o.AddEnv...)
	container.Env = newEnv

	if container.SecurityContext == nil {
		container.SecurityContext = &corev1.SecurityContext{}
	}
	switch {
	case o.AsNonRoot:
		b := true
		container.SecurityContext.RunAsNonRoot = &b
	case o.AsRoot:
		zero := int64(0)
		container.SecurityContext.RunAsUser = &zero
		container.SecurityContext.RunAsNonRoot = nil
	case o.AsUser != -1:
		container.SecurityContext.RunAsUser = &o.AsUser
		container.SecurityContext.RunAsNonRoot = nil
	}

	// if DebugOptions set container.SecurityContext.RunAsNonRoot to nil,
	// pod.Spec.SecurityContext.runAsNonRoot should be nil also.
	if container.SecurityContext.RunAsNonRoot == nil &&
		pod.Spec.SecurityContext != nil {
		pod.Spec.SecurityContext.RunAsNonRoot = nil
	}

	switch {
	case o.OneContainer:
		pod.Spec.InitContainers = nil
		pod.Spec.Containers = []corev1.Container{*container}
	case o.KeepInitContainers:
		// there is nothing we need to do
	case isInitContainer(pod, o.ContainerName):
		// keep only the init container we are debugging
		pod.Spec.InitContainers = []corev1.Container{*container}
	default:
		// clear all init containers
		pod.Spec.InitContainers = nil
	}

	clearHostPorts(pod)

	// keep workload annotations
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, containerResourcesAnnotationPrefix) ||
			strings.HasPrefix(k, podWorkloadTargetAnnotationPrefix) {
			annotations[k] = v
		}
	}

	// reset the pod
	if pod.Annotations == nil || !o.KeepAnnotations {
		pod.Annotations = make(map[string]string)
	}
	for k, v := range annotations {
		pod.Annotations[k] = v
	}
	if o.KeepLabels {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
	} else {
		pod.Labels = map[string]string{}
	}

	pod.ResourceVersion = ""
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	pod.Status = corev1.PodStatus{}
	pod.UID = ""
	pod.CreationTimestamp = metav1.Time{}

	// clear pod ownerRefs
	pod.ObjectMeta.OwnerReferences = []metav1.OwnerReference{}

	return pod, originalCommand
}

// createPod creates the debug pod, and will attempt to delete an existing debug
// pod with the same name, but will return an error in any other case.
func (o *DebugOptions) createPod(pod *corev1.Pod) (*corev1.Pod, error) {
	namespace, name := pod.Namespace, pod.Name

	// create the pod
	created, err := o.CoreClient.Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err == nil || !kapierrors.IsAlreadyExists(err) {
		return created, err
	}

	// only continue if the pod has the right annotations
	existing, err := o.CoreClient.Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if existing.Annotations[debugPodAnnotationSourceResource] != o.Annotations[debugPodAnnotationSourceResource] {
		return nil, fmt.Errorf("a pod already exists named %q, please delete it before running debug", name)
	}

	// delete the existing pod
	if err := o.CoreClient.Pods(namespace).Delete(context.TODO(), name, *metav1.NewDeleteOptions(0)); err != nil && !kapierrors.IsNotFound(err) {
		return nil, fmt.Errorf("unable to delete existing debug pod %q: %v", name, err)
	}
	return o.CoreClient.Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
}

func containerForName(pod *corev1.Pod, name string) *corev1.Container {
	for i, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i, c := range pod.Spec.Containers {
		if c.Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func isInitContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func containerNames(pod *corev1.Pod) []string {
	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

func clearHostPorts(pod *corev1.Pod) {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.HostNetwork {
			pod.Spec.InitContainers[i].Ports = nil
			continue
		}
		for j := range pod.Spec.InitContainers[i].Ports {
			if pod.Spec.InitContainers[i].Ports[j].HostPort > 0 {
				pod.Spec.InitContainers[i].Ports[j].HostPort = 0
			}
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.HostNetwork {
			pod.Spec.Containers[i].Ports = nil
			continue
		}
		for j := range pod.Spec.Containers[i].Ports {
			if pod.Spec.Containers[i].Ports[j].HostPort > 0 {
				pod.Spec.Containers[i].Ports[j].HostPort = 0
			}
		}
	}
}

func (o *DebugOptions) getContainerCommand() []string {
	if len(o.Command) == 1 && o.Command[0] == commandLinuxShell {
		pod := o.Attach.Pod

		if pod.Spec.OS != nil && pod.Spec.OS.Name == corev1.Windows {
			return []string{commandWindowsShell}
		}
	}

	return o.Command
}

func (o *DebugOptions) approximatePodTemplateForObject(object runtime.Object) (*corev1.PodTemplateSpec, error) {
	switch t := object.(type) {
	case *corev1.Node:
		o.IsNode = true
		if len(o.NodeName) > 0 {
			return nil, fmt.Errorf("you may not set --node-name when debugging a node")
		}
		if o.AsNonRoot || o.AsUser > 0 {
			// TODO: allow --as-root=false to skip all the namespaces except network
			return nil, fmt.Errorf("can't debug nodes without running as the root user")
		}
		image := o.Image
		if len(o.Image) == 0 {
			if t.Labels[corev1.LabelOSStable] == string(corev1.Windows) {
				return nil, fmt.Errorf("--image must be set when debugging Windows nodes")
			}
			imageStream := o.ImageStream
			if len(o.ImageStream) == 0 {
				imageStream = "openshift/tools:latest"
			}
			if imageFromStream, err := o.resolveImageStreamTagString(imageStream); err == nil {
				image = imageFromStream
			} else {
				klog.V(2).Infof("Unable to resolve image stream '%v': %v", imageStream, err)
			}
		}
		if len(image) == 0 {
			klog.V(2).Infof("Falling to 'registry.redhat.io/rhel9/support-tools' image")
			image = "registry.redhat.io/rhel9/support-tools"
		}
		zero := int64(0)
		isTrue := true
		hostPathType := corev1.HostPathDirectory
		template := &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"debug.openshift.io/managed-by": "oc-debug",
				},
			},
			Spec: corev1.PodSpec{
				NodeName:    t.Name,
				HostNetwork: true,
				HostPID:     true,
				HostIPC:     true,
				Volumes: []corev1.Volume{
					{
						Name: "host",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/",
								Type: &hostPathType,
							},
						},
					},
				},
				PriorityClassName: "openshift-user-critical",
				RestartPolicy:     corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{
						Name:  "container-00",
						Image: image,
						SecurityContext: &corev1.SecurityContext{
							Privileged: &isTrue,
							RunAsUser:  &zero,
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "host",
								MountPath: "/host",
							},
						},
						Env: []corev1.EnvVar{
							{
								// Set the Shell variable to auto-logout after 15m idle timeout
								Name:  "TMOUT",
								Value: "900",
							},
							{
								//  to collect more sos report requires this env var is set
								Name:  "HOST",
								Value: "/host",
							},
						},
					},
				},
			},
		}
		if t.Labels[corev1.LabelOSStable] == string(corev1.Windows) {
			template.Spec.OS = &corev1.PodOS{Name: corev1.Windows}
			template.Spec.HostPID = false
			template.Spec.HostIPC = false
			containerUser := "ContainerUser"
			template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
				WindowsOptions: &corev1.WindowsSecurityContextOptions{
					RunAsUserName: &containerUser,
				},
			}
		}
		return template, nil
	case *imagev1.ImageStreamTag:
		// create a minimal pod spec that uses the image referenced by the istag without any introspection
		// it possible that we could someday do a better job introspecting it
		return setNodeName(&corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{Name: "container-00", Image: t.Image.DockerImageReference},
				},
			},
		}, o.NodeName, o.NodeNameSet), nil
	case *imagev1.ImageStreamImage:
		// create a minimal pod spec that uses the image referenced by the istag without any introspection
		// it possible that we could someday do a better job introspecting it
		return setNodeName(&corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{
					{Name: "container-00", Image: t.Image.DockerImageReference},
				},
			},
		}, o.NodeName, o.NodeNameSet), nil
	case *appsv1.DeploymentConfig:
		fallback := t.Spec.Template

		latestDeploymentName := appsutil.LatestDeploymentNameForConfig(t)
		deployment, err := o.CoreClient.ReplicationControllers(t.Namespace).Get(context.TODO(), latestDeploymentName, metav1.GetOptions{})
		if err != nil {
			return setNodeName(fallback, o.NodeName, o.NodeNameSet), err
		}

		fallback = deployment.Spec.Template

		pods, err := o.CoreClient.Pods(deployment.Namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.SelectorFromSet(deployment.Spec.Selector).String()})
		if err != nil {
			return setNodeName(fallback, o.NodeName, o.NodeNameSet), err
		}

		// If we have any pods available, find the newest
		// pod with regards to our most recent deployment.
		// If the fallback PodTemplateSpec is nil, prefer
		// the newest pod available.
		for i := range pods.Items {
			pod := &pods.Items[i]
			if fallback == nil || pod.CreationTimestamp.Before(&fallback.CreationTimestamp) {
				fallback = &corev1.PodTemplateSpec{
					ObjectMeta: pod.ObjectMeta,
					Spec:       pod.Spec,
				}
			}
		}
		return setNodeName(fallback, o.NodeName, o.NodeNameSet), nil

	case *corev1.Pod:
		return setNodeName(&corev1.PodTemplateSpec{
			ObjectMeta: t.ObjectMeta,
			Spec:       t.Spec,
		}, o.NodeName, o.NodeNameSet), nil

	// ReplicationController
	case *corev1.ReplicationController:
		return setNodeName(t.Spec.Template, o.NodeName, o.NodeNameSet), nil

	// ReplicaSet
	case *extensionsv1beta1.ReplicaSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1beta2.ReplicaSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1.ReplicaSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil

	// Deployment
	case *extensionsv1beta1.Deployment:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1beta1.Deployment:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1beta2.Deployment:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1.Deployment:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil

	// StatefulSet
	case *kappsv1.StatefulSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1beta2.StatefulSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1beta1.StatefulSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil

	// DaemonSet
	case *extensionsv1beta1.DaemonSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1beta2.DaemonSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *kappsv1.DaemonSet:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil

	// Job
	case *batchv1.Job:
		return setNodeName(&t.Spec.Template, o.NodeName, o.NodeNameSet), nil

	// CronJob
	case *batchv1.CronJob:
		return setNodeName(&t.Spec.JobTemplate.Spec.Template, o.NodeName, o.NodeNameSet), nil
	case *batchv1beta1.CronJob:
		return setNodeName(&t.Spec.JobTemplate.Spec.Template, o.NodeName, o.NodeNameSet), nil
	}

	return nil, fmt.Errorf("%v is not supported by debug", reflect.TypeOf(object))
}

func (o *DebugOptions) getLogs(pod *corev1.Pod) error {
	return logs.LogsOptions{
		Object: pod,
		Options: &corev1.PodLogOptions{
			Container: o.ContainerName,
			Follow:    true,
		},
		RESTClientGetter: o.RESTClientGetter,
		ConsumeRequestFn: logs.DefaultConsumeRequest,
		IOStreams:        o.IOStreams,
		LogsForObject:    o.LogsForObject,
	}.RunLogs()
}

// getNamespace returns namespace name and clean up function.
// --to-namespace flag has the highest priority. If it is not set, -n flag is used.
// if there is no explicit namespace flag(--to-namespace, -n), default one is used.
// In the manner of node debugging, if default namespace is decided to be used and
// this namespace is not privileged, this function creates temporary namespace.
func (o *DebugOptions) getNamespace(infoNs string) (string, func(), error) {
	if len(o.ToNamespace) > 0 {
		return o.ToNamespace, func() {}, nil
	}

	if len(infoNs) == 0 {
		infoNs = o.Namespace
	}

	if o.ExplicitNamespace {
		return infoNs, func() {}, nil
	}

	if !o.IsNode || o.DryRun {
		return infoNs, func() {}, nil
	}

	currentNS, err := o.CoreClient.Namespaces().Get(context.TODO(), infoNs, metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}

	if val, ok := currentNS.Labels[api.EnforceLevelLabel]; !ok || val != string(api.LevelPrivileged) {
		tmpNS := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "openshift-debug-",
				Labels: map[string]string{
					api.EnforceLevelLabel:                            string(api.LevelPrivileged),
					api.AuditLevelLabel:                              string(api.LevelPrivileged),
					api.WarnLevelLabel:                               string(api.LevelPrivileged),
					"security.openshift.io/scc.podSecurityLabelSync": "false",
				},
				Annotations: map[string]string{
					"oc.openshift.io/command":    "oc debug",
					"openshift.io/node-selector": "",
				},
			},
		}

		ns, err := o.CoreClient.Namespaces().Create(context.TODO(), tmpNS, metav1.CreateOptions{})
		if err != nil {
			return "", nil, fmt.Errorf("unable to create temporary namespace %s: %v", tmpNS.Name, err)
		}

		if !o.Quiet {
			fmt.Fprintf(o.ErrOut, "Temporary namespace %s is created for debugging node...\n", ns.Name)
		}

		cleanup := func() {
			if o.PreservePod {
				return
			}
			if err := o.CoreClient.Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{}); err != nil {
				klog.V(2).Infof("Unable to delete temporary namespace %s: %v", ns.Name, err)
			} else {
				if !o.Quiet {
					fmt.Fprintf(o.ErrOut, "Temporary namespace %s was removed.\n", ns.Name)
				}
			}
		}

		return ns.Name, cleanup, nil
	}

	return infoNs, func() {}, nil
}

func setNodeName(template *corev1.PodTemplateSpec, nodeName string, overrideWhenEmpty bool) *corev1.PodTemplateSpec {
	if len(nodeName) > 0 || overrideWhenEmpty {
		template.Spec.NodeName = nodeName
	}
	return template
}

func (o *DebugOptions) resolveImageStreamTagString(s string) (string, error) {
	namespace, name, tag := parseImageStreamTagString(s)
	if len(namespace) == 0 {
		return "", fmt.Errorf("expected namespace/name:tag")
	}
	return o.resolveImageStreamTag(namespace, name, tag)
}

func parseImageStreamTagString(s string) (string, string, string) {
	var namespace, nameAndTag string
	parts := strings.SplitN(s, "/", 2)
	switch len(parts) {
	case 2:
		namespace = parts[0]
		nameAndTag = parts[1]
	case 1:
		nameAndTag = parts[0]
	}
	name, tag, _ := imageutil.SplitImageStreamTag(nameAndTag)
	return namespace, name, tag
}

func (o *DebugOptions) resolveImageStreamTag(namespace, name, tag string) (string, error) {
	imageStream, err := o.ImageClient.ImageStreams(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	var image string
	if image, _, _, _, err = imageutil.ResolveRecentPullSpecForTag(imageStream, tag, false); err != nil {
		return "", fmt.Errorf("unable to resolve the imagestream tag %s/%s:%s: %v", namespace, name, tag, err)
	}
	return image, nil
}
