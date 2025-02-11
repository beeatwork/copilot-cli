// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/aws/session"
	sdksecretsmanager "github.com/aws/aws-sdk-go/service/secretsmanager"
	sdkssm "github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/copilot-cli/cmd/copilot/template"
	"github.com/aws/copilot-cli/internal/pkg/aws/ecr"
	awsecs "github.com/aws/copilot-cli/internal/pkg/aws/ecs"
	"github.com/aws/copilot-cli/internal/pkg/aws/identity"
	"github.com/aws/copilot-cli/internal/pkg/aws/secretsmanager"
	"github.com/aws/copilot-cli/internal/pkg/aws/sessions"
	"github.com/aws/copilot-cli/internal/pkg/aws/ssm"
	clideploy "github.com/aws/copilot-cli/internal/pkg/cli/deploy"
	"github.com/aws/copilot-cli/internal/pkg/cli/group"
	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/docker/dockerengine"
	"github.com/aws/copilot-cli/internal/pkg/ecs"
	"github.com/aws/copilot-cli/internal/pkg/exec"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/repository"
	termcolor "github.com/aws/copilot-cli/internal/pkg/term/color"
	"github.com/aws/copilot-cli/internal/pkg/term/log"
	termprogress "github.com/aws/copilot-cli/internal/pkg/term/progress"
	"github.com/aws/copilot-cli/internal/pkg/term/prompt"
	"github.com/aws/copilot-cli/internal/pkg/term/selector"
	"github.com/aws/copilot-cli/internal/pkg/term/syncbuffer"
	"github.com/aws/copilot-cli/internal/pkg/workspace"
	"github.com/fatih/color"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

const (
	workloadAskPrompt = "Which workload would you like to run locally?"

	pauseContainerURI  = "public.ecr.aws/amazonlinux/amazonlinux:2023"
	pauseContainerName = "pause"
)

type runLocalVars struct {
	wkldName      string
	wkldType      string
	appName       string
	envName       string
	envOverrides  map[string]string
	portOverrides portOverrides
}

type runLocalOpts struct {
	runLocalVars

	sel             deploySelector
	ecsLocalClient  ecsLocalClient
	ssm             secretGetter
	secretsManager  secretGetter
	sessProvider    sessionProvider
	sess            *session.Session
	envSess         *session.Session
	targetEnv       *config.Environment
	targetApp       *config.Application
	store           store
	ws              wsWlDirReader
	cmd             execRunner
	dockerEngine    dockerEngineRunner
	repository      repositoryService
	containerSuffix string
	newColor        func() *color.Color
	prog            progress

	buildContainerImages func(mft manifest.DynamicWorkload) (map[string]string, error)
	configureClients     func(o *runLocalOpts) error
	labeledTermPrinter   func(fw syncbuffer.FileWriter, bufs []*syncbuffer.LabeledSyncBuffer, opts ...syncbuffer.LabeledTermPrinterOption) clideploy.LabeledTermPrinter
	unmarshal            func([]byte) (manifest.DynamicWorkload, error)
	newInterpolator      func(app, env string) interpolator
}

func newRunLocalOpts(vars runLocalVars) (*runLocalOpts, error) {
	sessProvider := sessions.ImmutableProvider(sessions.UserAgentExtras("run local"))
	defaultSess, err := sessProvider.Default()
	if err != nil {
		return nil, err
	}

	store := config.NewSSMStore(identity.New(defaultSess), sdkssm.New(defaultSess), aws.StringValue(defaultSess.Config.Region))
	deployStore, err := deploy.NewStore(sessProvider, store)
	if err != nil {
		return nil, err
	}

	ws, err := workspace.Use(afero.NewOsFs())
	if err != nil {
		return nil, err
	}
	labeledTermPrinter := func(fw syncbuffer.FileWriter, bufs []*syncbuffer.LabeledSyncBuffer, opts ...syncbuffer.LabeledTermPrinterOption) clideploy.LabeledTermPrinter {
		return syncbuffer.NewLabeledTermPrinter(fw, bufs, opts...)
	}
	opts := &runLocalOpts{
		runLocalVars:       vars,
		sel:                selector.NewDeploySelect(prompt.New(), store, deployStore),
		store:              store,
		ws:                 ws,
		newInterpolator:    newManifestInterpolator,
		sessProvider:       sessProvider,
		unmarshal:          manifest.UnmarshalWorkload,
		sess:               defaultSess,
		cmd:                exec.NewCmd(),
		dockerEngine:       dockerengine.New(exec.NewCmd()),
		labeledTermPrinter: labeledTermPrinter,
		newColor:           termcolor.ColorGenerator(),
		prog:               termprogress.NewSpinner(log.DiagnosticWriter),
	}
	opts.configureClients = func(o *runLocalOpts) error {
		defaultSessEnvRegion, err := o.sessProvider.DefaultWithRegion(o.targetEnv.Region)
		if err != nil {
			return fmt.Errorf("create default session with region %s: %w", o.targetEnv.Region, err)
		}
		o.envSess, err = o.sessProvider.FromRole(o.targetEnv.ManagerRoleARN, o.targetEnv.Region)
		if err != nil {
			return fmt.Errorf("create env session %s: %w", o.targetEnv.Region, err)
		}

		// EnvManagerRole has permissions to get task def and get SSM values.
		// However, it doesn't have permissions to get secrets from secrets manager,
		// so use the default sess and *hope* they have permissions.
		o.ecsLocalClient = ecs.New(o.envSess)
		o.ssm = ssm.New(o.envSess)
		o.secretsManager = secretsmanager.New(defaultSessEnvRegion)

		resources, err := cloudformation.New(o.sess, cloudformation.WithProgressTracker(os.Stderr)).GetAppResourcesByRegion(o.targetApp, o.targetEnv.Region)
		if err != nil {
			return fmt.Errorf("get application %s resources from region %s: %w", o.appName, o.envName, err)
		}
		repoName := clideploy.RepoName(o.appName, o.wkldName)
		o.repository = repository.NewWithURI(ecr.New(defaultSessEnvRegion), repoName, resources.RepositoryURLs[o.wkldName])
		return nil
	}
	opts.buildContainerImages = func(mft manifest.DynamicWorkload) (map[string]string, error) {
		gitShortCommit := imageTagFromGit(opts.cmd)
		image := clideploy.ContainerImageIdentifier{
			GitShortCommitTag: gitShortCommit,
		}
		out := &clideploy.UploadArtifactsOutput{}
		if err := clideploy.BuildContainerImages(&clideploy.ImageActionInput{
			Name:               opts.wkldName,
			WorkspacePath:      opts.ws.Path(),
			Image:              image,
			Mft:                mft.Manifest(),
			GitShortCommitTag:  gitShortCommit,
			Builder:            opts.repository,
			Login:              opts.repository.Login,
			CheckDockerEngine:  opts.dockerEngine.CheckDockerEngineRunning,
			LabeledTermPrinter: opts.labeledTermPrinter,
		}, out); err != nil {
			return nil, err
		}

		containerURIs := make(map[string]string, len(out.ImageDigests))
		for name, info := range out.ImageDigests {
			if len(info.RepoTags) == 0 {
				// this shouldn't happen, but just to avoid a panic in case
				return nil, fmt.Errorf("no repo tags for image %q", name)
			}
			containerURIs[name] = info.RepoTags[0]
		}
		return containerURIs, nil
	}
	return opts, nil
}

// Validate returns an error for any invalid optional flags.
func (o *runLocalOpts) Validate() error {
	if o.appName == "" {
		return errNoAppInWorkspace
	}
	// Ensure that the application name provided exists in the workspace
	app, err := o.store.GetApplication(o.appName)
	if err != nil {
		return fmt.Errorf("get application %s: %w", o.appName, err)
	}
	o.targetApp = app
	return nil
}

// Ask prompts the user for any unprovided required fields and validates them.
func (o *runLocalOpts) Ask() error {
	return o.validateAndAskWkldEnvName()
}

func (o *runLocalOpts) validateAndAskWkldEnvName() error {
	if o.envName != "" {
		env, err := o.store.GetEnvironment(o.appName, o.envName)
		if err != nil {
			return err
		}
		o.targetEnv = env
	}
	if o.wkldName != "" {
		if _, err := o.store.GetWorkload(o.appName, o.wkldName); err != nil {
			return err
		}
	}

	deployedWorkload, err := o.sel.DeployedWorkload(workloadAskPrompt, "", o.appName, selector.WithEnv(o.envName), selector.WithName(o.wkldName))
	if err != nil {
		return fmt.Errorf("select a deployed workload from application %s: %w", o.appName, err)
	}
	if o.envName == "" {
		env, err := o.store.GetEnvironment(o.appName, deployedWorkload.Env)
		if err != nil {
			return fmt.Errorf("get environment %q configuration: %w", o.envName, err)
		}
		o.targetEnv = env
	}

	o.wkldName = deployedWorkload.Name
	o.envName = deployedWorkload.Env
	o.wkldType = deployedWorkload.Type
	o.containerSuffix = o.getContainerSuffix()
	return nil
}

// Execute builds and runs the workload images locally.
func (o *runLocalOpts) Execute() error {
	if err := o.configureClients(o); err != nil {
		return err
	}

	ctx := context.Background()

	taskDef, err := o.ecsLocalClient.TaskDefinition(o.appName, o.envName, o.wkldName)
	if err != nil {
		return fmt.Errorf("get task definition: %w", err)
	}

	envVars, err := o.getEnvVars(ctx, taskDef)
	if err != nil {
		return fmt.Errorf("get env vars: %w", err)
	}

	// map of containerPort -> hostPort
	ports := make(map[string]string)
	for _, container := range taskDef.ContainerDefinitions {
		for _, mapping := range container.PortMappings {
			host := strconv.FormatInt(aws.Int64Value(mapping.HostPort), 10)

			ctr := host
			if mapping.ContainerPort != nil {
				ctr = strconv.FormatInt(aws.Int64Value(mapping.ContainerPort), 10)
			}
			ports[ctr] = host
		}
	}
	for _, port := range o.portOverrides {
		ports[port.container] = port.host
	}

	mft, err := workloadManifest(&workloadManifestInput{
		name:         o.wkldName,
		appName:      o.appName,
		envName:      o.envName,
		interpolator: o.newInterpolator(o.appName, o.envName),
		ws:           o.ws,
		unmarshal:    o.unmarshal,
		sess:         o.envSess,
	})
	if err != nil {
		return err
	}

	containerURIs, err := o.buildContainerImages(mft)
	if err != nil {
		return fmt.Errorf("build images: %w", err)
	}

	// fill the location from the task def for containers without a URI
	for _, container := range taskDef.ContainerDefinitions {
		name := aws.StringValue(container.Name)
		if _, ok := containerURIs[name]; !ok {
			containerURIs[name] = aws.StringValue(container.Image)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)
	gotSigInt := &atomic.Bool{}

	g.Go(func() error {
		defer cancel() // needed in case all containers exit successfully

		if err := o.runPauseContainer(ctx, ports); err != nil {
			// if we've received a sigint, we want to ignore
			// any errors coming from this goroutine
			if gotSigInt.Load() {
				return nil
			}
			return fmt.Errorf("run pause container: %w", err)
		}

		err := o.runContainers(ctx, containerURIs, envVars)
		if gotSigInt.Load() {
			return nil
		}
		return err
	})

	g.Go(func() error {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		select {
		case <-ctx.Done():
		case <-sigCh:
			gotSigInt.Store(true)
			// reset signal handler in case we get ctrl+c again
			// while trying to stop containers
			signal.Stop(sigCh)
			fmt.Printf("\nStopping containers...\n\n")
		}

		return o.cleanUpContainers(context.Background(), containerURIs)
	})

	return g.Wait()
}

func (o *runLocalOpts) getContainerSuffix() string {
	return fmt.Sprintf("%s-%s-%s", o.appName, o.envName, o.wkldName)
}

func (o *runLocalOpts) runPauseContainer(ctx context.Context, ports map[string]string) error {
	// flip ports to be host->ctr
	flippedPorts := make(map[string]string, len(ports))
	for k, v := range ports {
		flippedPorts[v] = k
	}
	containerNameWithSuffix := fmt.Sprintf("%s-%s", pauseContainerName, o.containerSuffix)
	runOptions := &dockerengine.RunOptions{
		ImageURI:       pauseContainerURI,
		ContainerName:  containerNameWithSuffix,
		ContainerPorts: flippedPorts,
		Command:        []string{"sleep", "infinity"},
		LogOptions: dockerengine.RunLogOptions{
			Color:      o.newColor(),
			LinePrefix: "[pause] ",
		},
	}

	//channel to receive any error from the goroutine
	errCh := make(chan error, 1)

	go func() {
		if err := o.dockerEngine.Run(ctx, runOptions); err != nil {
			errCh <- err
		}
	}()

	// go routine to check if pause container is running
	go func() {
		for {
			isRunning, err := o.dockerEngine.IsContainerRunning(containerNameWithSuffix)
			if err != nil {
				errCh <- fmt.Errorf("check if container is running: %w", err)
				return
			}
			if isRunning {
				errCh <- nil
				return
			}
			// If the container isn't running yet, sleep for a short duration before checking again.
			time.Sleep(time.Second)
		}
	}()
	err := <-errCh
	if err != nil {
		return err
	}

	return nil
}

func (o *runLocalOpts) runContainers(ctx context.Context, containerURIs map[string]string, envVars map[string]containerEnv) error {
	g, ctx := errgroup.WithContext(ctx)
	for name, uri := range containerURIs {
		name := name
		uri := uri

		vars, secrets := make(map[string]string), make(map[string]string)
		for k, v := range envVars[name] {
			if v.Secret {
				secrets[k] = v.Value
			} else {
				vars[k] = v.Value
			}
		}

		// Execute each container run in a separate goroutine
		g.Go(func() error {
			runOptions := &dockerengine.RunOptions{
				ImageURI:         uri,
				ContainerName:    fmt.Sprintf("%s-%s", name, o.containerSuffix),
				Secrets:          secrets,
				EnvVars:          vars,
				ContainerNetwork: fmt.Sprintf("%s-%s", pauseContainerName, o.containerSuffix),
				LogOptions: dockerengine.RunLogOptions{
					Color:      o.newColor(),
					LinePrefix: fmt.Sprintf("[%s] ", name),
				},
			}
			if err := o.dockerEngine.Run(ctx, runOptions); err != nil {
				return fmt.Errorf("run container %q: %w", name, err)
			}
			return nil
		})
	}

	return g.Wait()
}

func (o *runLocalOpts) cleanUpContainers(ctx context.Context, containerURIs map[string]string) error {
	cleanUp := func(id string) error {
		o.prog.Start(fmt.Sprintf("Stopping %q", id))
		if err := o.dockerEngine.Stop(id); err != nil {
			o.prog.Stop(log.Serrorf("Failed to stop %q\n", id))
			return fmt.Errorf("stop: %w", err)
		}

		o.prog.Start(fmt.Sprintf("Removing %q", id))
		if err := o.dockerEngine.Rm(id); err != nil {
			o.prog.Stop(log.Serrorf("Failed to remove %q\n", id))
			return fmt.Errorf("rm: %w", err)
		}

		o.prog.Stop(log.Ssuccessf("Cleaned up %q\n", id))
		return nil
	}

	var errs []error

	for name := range containerURIs {
		ctr := fmt.Sprintf("%s-%s", name, o.containerSuffix)
		if err := cleanUp(ctr); err != nil {
			errs = append(errs, fmt.Errorf("clean up %q: %w", ctr, err))
		}
	}

	pauseCtr := fmt.Sprintf("%s-%s", pauseContainerName, o.containerSuffix)
	if err := cleanUp(pauseCtr); err != nil {
		errs = append(errs, fmt.Errorf("clean up %q: %w", pauseCtr, err))
	}

	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool {
			return errs[i].Error() < errs[j].Error()
		})
		return errors.Join(errs...)
	}
	return nil
}

type containerEnv map[string]envVarValue

type envVarValue struct {
	Value    string
	Secret   bool
	Override bool
}

// getEnvVars uses env overrides passed by flags and environment variables/secrets
// specified in the Task Definition to return a set of environment varibles for each
// continer defined in the TaskDefinition. The returned map is a map of container names,
// each of which contains a mapping of key->envVarValue, which defines if the variable is a secret or not.
func (o *runLocalOpts) getEnvVars(ctx context.Context, taskDef *awsecs.TaskDefinition) (map[string]containerEnv, error) {
	creds, err := o.sess.Config.Credentials.GetWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("get IAM credentials: %w", err)
	}

	envVars := make(map[string]containerEnv)
	for _, ctr := range taskDef.ContainerDefinitions {
		name := aws.StringValue(ctr.Name)
		envVars[name] = map[string]envVarValue{
			"AWS_ACCESS_KEY_ID": {
				Value: creds.AccessKeyID,
			},
			"AWS_SECRET_ACCESS_KEY": {
				Value: creds.SecretAccessKey,
			},
			"AWS_SESSION_TOKEN": {
				Value: creds.SessionToken,
			},
		}
		if o.sess.Config.Region != nil {
			val := envVarValue{
				Value: aws.StringValue(o.sess.Config.Region),
			}
			envVars[name]["AWS_DEFAULT_REGION"] = val
			envVars[name]["AWS_REGION"] = val
		}
	}

	for _, e := range taskDef.EnvironmentVariables() {
		envVars[e.Container][e.Name] = envVarValue{
			Value: e.Value,
		}
	}

	if err := o.fillEnvOverrides(envVars); err != nil {
		return nil, fmt.Errorf("parse env overrides: %w", err)
	}

	if err := o.fillSecrets(ctx, envVars, taskDef); err != nil {
		return nil, fmt.Errorf("get secrets: %w", err)
	}
	return envVars, nil
}

// fillEnvOverrides parses environment variable overrides passed via flag.
// The expected format of the flag values is KEY=VALUE, with an optional container name
// in the format of [containerName]:KEY=VALUE. If the container name is omitted,
// the environment variable override is applied to all containers in the task definition.
func (o *runLocalOpts) fillEnvOverrides(envVars map[string]containerEnv) error {
	for k, v := range o.envOverrides {
		if !strings.Contains(k, ":") {
			// apply override to all containers
			for ctr := range envVars {
				envVars[ctr][k] = envVarValue{
					Value:    v,
					Override: true,
				}
			}
			continue
		}

		// only apply override to the specified container
		split := strings.SplitN(k, ":", 2)
		ctr, key := split[0], split[1] // len(split) will always be 2 since we know there is a ":"
		if _, ok := envVars[ctr]; !ok {
			return fmt.Errorf("%q targets invalid container", k)
		}
		envVars[ctr][key] = envVarValue{
			Value:    v,
			Override: true,
		}
	}

	return nil
}

// fillSecrets collects non-overridden secrets from the task definition and
// makes requests to SSM and Secrets Manager to get their value.
func (o *runLocalOpts) fillSecrets(ctx context.Context, envVars map[string]containerEnv, taskDef *awsecs.TaskDefinition) error {
	// figure out which secrets we need to get, set value to ValueFrom
	unique := make(map[string]string)
	for _, s := range taskDef.Secrets() {
		cur, ok := envVars[s.Container][s.Name]
		if cur.Override {
			// ignore secrets that were overridden
			continue
		}
		if ok {
			return fmt.Errorf("secret names must be unique, but an environment variable %q already exists", s.Name)
		}

		envVars[s.Container][s.Name] = envVarValue{
			Value:  s.ValueFrom,
			Secret: true,
		}
		unique[s.ValueFrom] = ""
	}

	// get value of all needed secrets
	g, ctx := errgroup.WithContext(ctx)
	mu := &sync.Mutex{}
	mu.Lock() // lock until finished ranging over unique
	for valueFrom := range unique {
		valueFrom := valueFrom
		g.Go(func() error {
			val, err := o.getSecret(ctx, valueFrom)
			if err != nil {
				return fmt.Errorf("get secret %q: %w", valueFrom, err)
			}

			mu.Lock()
			defer mu.Unlock()
			unique[valueFrom] = val
			return nil
		})
	}
	mu.Unlock()
	if err := g.Wait(); err != nil {
		return err
	}

	// replace secrets with resolved values
	for ctr, vars := range envVars {
		for key, val := range vars {
			if val.Secret {
				envVars[ctr][key] = envVarValue{
					Value:  unique[val.Value],
					Secret: true,
				}
			}
		}
	}

	return nil
}

func (o *runLocalOpts) getSecret(ctx context.Context, valueFrom string) (string, error) {
	// SSM secrets can be specified as parameter name instead of an ARN.
	// Default to ssm if valueFrom is not an ARN.
	getter := o.ssm
	if parsed, err := arn.Parse(valueFrom); err == nil { // only overwrite if successful
		switch parsed.Service {
		case sdkssm.ServiceName:
			getter = o.ssm
		case sdksecretsmanager.ServiceName:
			getter = o.secretsManager
		default:
			return "", fmt.Errorf("invalid ARN; not a SSM or Secrets Manager ARN")
		}
	}

	return getter.GetSecretValue(ctx, valueFrom)
}

// BuildRunLocalCmd builds the command for running a workload locally
func BuildRunLocalCmd() *cobra.Command {
	vars := runLocalVars{}
	cmd := &cobra.Command{
		Use:   "run local",
		Short: "Run the workload locally.",
		Long:  "Run the workload locally.",
		RunE: runCmdE(func(cmd *cobra.Command, args []string) error {
			opts, err := newRunLocalOpts(vars)
			if err != nil {
				return err
			}
			return run(opts)
		}),
		Annotations: map[string]string{
			"group": group.Develop,
		},
	}
	cmd.SetUsageTemplate(template.Usage)

	cmd.Flags().StringVarP(&vars.wkldName, nameFlag, nameFlagShort, "", workloadFlagDescription)
	cmd.Flags().StringVarP(&vars.envName, envFlag, envFlagShort, "", envFlagDescription)
	cmd.Flags().StringVarP(&vars.appName, appFlag, appFlagShort, tryReadingAppName(), appFlagDescription)
	cmd.Flags().Var(&vars.portOverrides, portOverrideFlag, portOverridesFlagDescription)
	cmd.Flags().StringToStringVar(&vars.envOverrides, envVarOverrideFlag, nil, envVarOverrideFlagDescription)
	return cmd
}
