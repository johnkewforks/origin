package cmd

import (
	"fmt"
	"io"
	"os"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	ctl "github.com/GoogleCloudPlatform/kubernetes/pkg/kubectl"
	cmdutil "github.com/GoogleCloudPlatform/kubernetes/pkg/kubectl/cmd/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/errors"
	"github.com/golang/glog"
	"github.com/spf13/cobra"

	buildapi "github.com/openshift/origin/pkg/build/api"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"
	dockerutil "github.com/openshift/origin/pkg/cmd/util/docker"
	configcmd "github.com/openshift/origin/pkg/config/cmd"
	newcmd "github.com/openshift/origin/pkg/generate/app/cmd"
	imageapi "github.com/openshift/origin/pkg/image/api"
	"github.com/openshift/origin/pkg/util"
)

type usage interface {
	UsageError(commandName string) string
}

var errExit = fmt.Errorf("exit directly")

const newAppLongDesc = `
Create a new application in OpenShift by specifying source code, templates, and/or images.

This command will try to build up the components of an application using images, templates, 
or code located on your system. It will lookup the images on the local Docker installation 
(if available), a Docker registry, or an OpenShift image stream. If you specify a source
code URL, it will set up a build that takes your source code and converts it into an
image that can run inside of a pod. The images will be deployed via a deployment
configuration, and a service will be hooked up to the first public port of the app.

Examples:

	# Try to create an application based on the source code in the current directory
	$ %[1]s new-app .

	$ Use the public Docker Hub MySQL image to create an app
	$ %[1]s new-app mysql

	# Use a MySQL image in a private registry to create an app
	$ %[1]s new-app myregistry.com/mycompany/mysql

	# Create an application from the remote repository using the specified label
	$ %[1]s new-app https://github.com/openshift/ruby-hello-world -l name=hello-world

	# Create an application based on a stored template, explicitly setting a parameter value
	$ %[1]s new-app ruby-helloworld-sample --env=MYSQL_USER=admin

If you specify source code, you may need to run a build with 'start-build' after the
application is created.

ALPHA: This command is under active development - feedback is appreciated.
`

// NewCmdNewApplication implements the OpenShift cli new-app command
func NewCmdNewApplication(fullName string, f *clientcmd.Factory, out io.Writer) *cobra.Command {
	_, typer := f.Object()
	config := newcmd.NewAppConfig(typer)
	helper := dockerutil.NewHelper()

	cmd := &cobra.Command{
		Use:   "new-app <components> [--code=<path|url>]",
		Short: "Create a new application",
		Long:  fmt.Sprintf(newAppLongDesc, fullName),

		Run: func(c *cobra.Command, args []string) {
			err := RunNewApplication(f, out, c, args, config, helper)
			if err == errExit {
				os.Exit(1)
			}
			cmdutil.CheckErr(err)
		},
	}

	cmd.Flags().Var(&config.SourceRepositories, "code", "Source code to use to build this application.")
	cmd.Flags().VarP(&config.ImageStreams, "image", "i", "Name of an OpenShift image stream to use in the app.")
	cmd.Flags().Var(&config.DockerImages, "docker-image", "Name of a Docker image to include in the app.")
	cmd.Flags().Var(&config.Templates, "template", "Name of an OpenShift stored template to use in the app.")
	cmd.Flags().VarP(&config.TemplateParameters, "param", "p", "Specify a list of key value pairs (eg. -p FOO=BAR,BAR=FOO) to set/override parameter values in the template.")
	cmd.Flags().Var(&config.Groups, "group", "Indicate components that should be grouped together as <comp1>+<comp2>.")
	cmd.Flags().VarP(&config.Environment, "env", "e", "Specify key value pairs of environment variables to set into each container.")
	cmd.Flags().StringVar(&config.TypeOfBuild, "build", "", "Specify the type of build to use if you don't want to detect (docker|source).")
	cmd.Flags().StringP("labels", "l", "", "Label to set in all resources for this application.")

	// TODO AddPrinterFlags disabled so that it doesn't conflict with our own "template" flag.
	// Need a better solution.
	// cmdutil.AddPrinterFlags(cmd)
	cmd.Flags().StringP("output", "o", "", "Output format. One of: json|yaml|template|templatefile.")
	cmd.Flags().String("output-version", "", "Output the formatted object with the given version (default api-version).")
	cmd.Flags().Bool("no-headers", false, "When using the default output, don't print headers.")
	cmd.Flags().String("output-template", "", "Template string or path to template file to use when -o=template or -o=templatefile.  The template format is golang templates [http://golang.org/pkg/text/template/#pkg-overview]")

	return cmd
}

// RunNewApplication contains all the necessary functionality for the OpenShift cli new-app command
func RunNewApplication(f *clientcmd.Factory, out io.Writer, c *cobra.Command, args []string, config *newcmd.AppConfig, helper *dockerutil.Helper) error {
	namespace, err := f.DefaultNamespace()

	if err != nil {
		return err
	}

	if dockerClient, _, err := helper.GetClient(); err == nil {
		if err := dockerClient.Ping(); err == nil {
			config.SetDockerClient(dockerClient)
		} else {
			glog.V(2).Infof("No local Docker daemon detected: %v", err)
		}
	}

	osclient, _, err := f.Clients()
	if err != nil {
		return err
	}
	config.SetOpenShiftClient(osclient, namespace)

	unknown := config.AddArguments(args)
	if len(unknown) != 0 {
		return cmdutil.UsageError(c, "Did not recognize the following arguments: %v", unknown)
	}

	result, err := config.Run(out)
	if err != nil {
		if errs, ok := err.(errors.Aggregate); ok {
			if len(errs.Errors()) == 1 {
				err = errs.Errors()[0]
			}
		}
		if err == newcmd.ErrNoInputs {
			// TODO: suggest things to the user
			return cmdutil.UsageError(c, "You must specify one or more images, image streams, templates or source code locations to create an application.")
		}
		return err
	}

	label := cmdutil.GetFlagString(c, "labels")
	if len(label) != 0 {
		lbl := ctl.ParseLabels(label)
		for _, object := range result.List.Items {
			err = util.AddObjectLabels(object, lbl)
			if err != nil {
				return err
			}
		}
	}

	if len(cmdutil.GetFlagString(c, "output")) != 0 {
		return f.Factory.PrintObject(c, result.List, out)
	}

	bulk := configcmd.Bulk{
		Factory: f.Factory,
		After:   configcmd.NewPrintNameOrErrorAfter(out, os.Stderr),
	}
	if errs := bulk.Create(result.List, namespace); len(errs) != 0 {
		return errExit
	}

	hasMissingRepo := false
	for _, item := range result.List.Items {
		switch t := item.(type) {
		case *kapi.Service:
			// TODO: handle multi-port created services
			fmt.Fprintf(c.Out(), "Service %q created at %s:%d to talk to pods over port %d.\n", t.Name, t.Spec.PortalIP, t.Spec.Ports[0].Port, t.Spec.Ports[0].TargetPort.IntVal)
		case *buildapi.BuildConfig:
			fmt.Fprintf(c.Out(), "A build was created - you can run `osc start-build %s` to start it.\n", t.Name)
		case *imageapi.ImageStream:
			if len(t.Status.DockerImageRepository) == 0 {
				if hasMissingRepo {
					continue
				}
				hasMissingRepo = true
				fmt.Fprintf(c.Out(), "WARNING: We created an image stream %q, but it does not look like a Docker registry has been integrated with the OpenShift server. Automatic builds and deployments depend on that integration to detect new images and will not function properly.\n", t.Name)
			}
		}
	}
	return nil
}
