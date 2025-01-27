/*
Copyright 2021-2023 Red Hat, Inc.

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

package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/authn"
	pacv1alpha1 "github.com/openshift-pipelines/pipelines-as-code/pkg/apis/pipelinesascode/v1alpha1"
	routev1 "github.com/openshift/api/route/v1"
	appstudiov1alpha1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	"github.com/redhat-appstudio/application-service/gitops"
	gitopsprepare "github.com/redhat-appstudio/application-service/gitops/prepare"
	"github.com/redhat-appstudio/application-service/pkg/devfile"
	"github.com/redhat-appstudio/build-service/pkg/boerrors"
	"github.com/redhat-appstudio/build-service/pkg/github"
	"github.com/redhat-appstudio/build-service/pkg/gitlab"
	l "github.com/redhat-appstudio/build-service/pkg/logs"
	tektonapi "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	oci "github.com/tektoncd/pipeline/pkg/remote/oci"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	gogithub "github.com/google/go-github/v45/github"
	gogitlab "github.com/xanzy/go-gitlab"
)

const (
	pipelineRunOnPushSuffix          = "-on-push"
	pipelineRunOnPRSuffix            = "-on-pull-request"
	pipelineRunOnPushFilename        = "push.yaml"
	pipelineRunOnPRFilename          = "pull-request.yaml"
	pipelineRunOnPRExpirationEnvVar  = "IMAGE_TAG_ON_PR_EXPIRATION"
	pipelineRunOnPRExpirationDefault = "5d"
	pipelinesAsCodeNamespace         = "openshift-pipelines"
	pipelinesAsCodeNamespaceFallback = "pipelines-as-code"
	pipelinesAsCodeRouteName         = "pipelines-as-code-controller"
	pipelinesAsCodeRouteEnvVar       = "PAC_WEBHOOK_URL"

	pacMergeRequestSourceBranchPrefix = "appstudio-"

	defaultPipelineName   = "docker-build"
	defaultPipelineBundle = "quay.io/redhat-appstudio-tekton-catalog/pipeline-docker-build:8cf8982d58a841922b687b7166f0cfdc1cc3fc72"

	mergeRequestDescription = `
# Pipelines as Code configuration proposal

To start the PipelineRun, add a new comment with content ` + "`/ok-to-test`" + `

For more detailed information about running a PipelineRun, please refer to Pipelines as Code documentation [Running the PipelineRun](https://pipelinesascode.com/docs/guide/running/)

To customize the proposed PipelineRuns after merge, please refer to [Build Pipeline customization](https://redhat-appstudio.github.io/docs.stonesoup.io/Documentation/main/getting-started/build_service.html)
`
)

// ProvisionPaCForComponent does Pipelines as Code provision for the given component.
// Mainly, it creates PaC configuration merge request into the component source repositotiry.
// If GitHub PaC application is not configured, creates a webhook for PaC.
func (r *ComponentBuildReconciler) ProvisionPaCForComponent(ctx context.Context, component *appstudiov1alpha1.Component) error {
	log := ctrllog.FromContext(ctx).WithName("PaC-setup")
	ctx = ctrllog.IntoContext(ctx, log)

	gitProvider, err := gitops.GetGitProvider(*component)
	if err != nil {
		// Do not reconcile, because configuration must be fixed before it is possible to proceed.
		return boerrors.NewBuildOpError(boerrors.EUnknownGitProvider,
			fmt.Errorf("error detecting git provider: %w", err))
	}

	pacSecret, err := r.ensurePaCSecret(ctx, component, gitProvider)
	if err != nil {
		return err
	}

	if err := validatePaCConfiguration(gitProvider, pacSecret.Data); err != nil {
		r.EventRecorder.Event(pacSecret, "Warning", "ErrorValidatingPaCSecret", err.Error())
		// Do not reconcile, because configuration must be fixed before it is possible to proceed.
		return boerrors.NewBuildOpError(boerrors.EPaCSecretInvalid,
			fmt.Errorf("invalid configuration in Pipelines as Code secret: %w", err))
	}

	var webhookSecretString, webhookTargetUrl string
	if !gitops.IsPaCApplicationConfigured(gitProvider, pacSecret.Data) {
		// Generate webhook secret for the component git repository if not yet generated
		// and stores it in the corresponding k8s secret.
		webhookSecretString, err = r.ensureWebhookSecret(ctx, component)
		if err != nil {
			return err
		}

		// Obtain Pipelines as Code callback URL
		webhookTargetUrl, err = r.getPaCWebhookTargetUrl(ctx)
		if err != nil {
			return err
		}
	}

	if err := r.ensurePaCRepository(ctx, component, pacSecret.Data); err != nil {
		return err
	}

	// Manage merge request for Pipelines as Code configuration
	mrUrl, err := r.ConfigureRepositoryForPaC(ctx, component, pacSecret.Data, webhookTargetUrl, webhookSecretString)
	if err != nil {
		r.EventRecorder.Event(component, "Warning", "ErrorConfiguringPaCForComponentRepository", err.Error())
		return err
	}
	var mrMessage string
	if mrUrl != "" {
		mrMessage = fmt.Sprintf("Pipelines as Code configuration merge request: %s", mrUrl)
	} else {
		mrMessage = "Pipelines as Code configuration is up to date"
	}
	log.Info(mrMessage)
	r.EventRecorder.Event(component, "Normal", "PipelinesAsCodeConfiguration", mrMessage)

	if mrUrl != "" {
		// PaC PR has been just created
		pipelinesAsCodeComponentProvisionTimeMetric.Observe(time.Since(component.CreationTimestamp.Time).Seconds())
	}

	return nil
}

// UndoPaCProvisionForComponent creates merge request that removes Pipelines as Code configuration from component source repository.
// Deletes PaC webhook if used.
// In case of any errors just logs them and does not block Component deletion.
func (r *ComponentBuildReconciler) UndoPaCProvisionForComponent(ctx context.Context, component *appstudiov1alpha1.Component) {
	log := ctrllog.FromContext(ctx).WithName("PaC-cleanup")
	ctx = ctrllog.IntoContext(ctx, log)

	gitProvider, err := gitops.GetGitProvider(*component)
	if err != nil {
		log.Error(err, "error detecting git provider")
		// There is no point to continue if git provider is not known.
		return
	}

	pacSecret := corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: buildServiceNamespaceName, Name: gitopsprepare.PipelinesAsCodeSecretName}, &pacSecret); err != nil {
		log.Error(err, "error getting git provider credentials secret", l.Action, l.ActionView)
		// Cannot continue without accessing git provider credentials.
		return
	}

	webhookTargetUrl := ""
	if !gitops.IsPaCApplicationConfigured(gitProvider, pacSecret.Data) {
		webhookTargetUrl, err = r.getPaCWebhookTargetUrl(ctx)
		if err != nil {
			// Just log the error and continue with merge request creation
			log.Error(err, "failed to get Pipelines as Code webhook target URL", l.Action, l.ActionView)
		}
	}

	// Manage merge request for Pipelines as Code configuration removal
	mrUrl, action, err := r.UnconfigureRepositoryForPaC(ctx, component, pacSecret.Data, webhookTargetUrl)
	if err != nil {
		log.Error(err, "failed to create merge request to remove Pipelines as Code configuration from Component source repository", l.Audit, "true")
		return
	}
	if action == "delete" {
		if mrUrl != "" {
			log.Info(fmt.Sprintf("Pipelines as Code configuration removal merge request: %s", mrUrl))
		} else {
			log.Info("Pipelines as Code configuration removal merge request is not needed")
		}
	} else if action == "close" {
		log.Info(fmt.Sprintf("Pipelines as Code configuration merge request has been closed: %s", mrUrl))
	}
}

func (r *ComponentBuildReconciler) ensurePaCSecret(ctx context.Context, component *appstudiov1alpha1.Component, gitProvider string) (*corev1.Secret, error) {
	// Expected that the secret contains token for Pipelines as Code webhook configuration,
	// but under <git-provider>.token field. For example: github.token
	// Also it can contain github-private-key and github-application-id
	// in case GitHub Application is used instead of webhook.
	pacSecret := corev1.Secret{}
	pacSecretKey := types.NamespacedName{Namespace: component.Namespace, Name: gitopsprepare.PipelinesAsCodeSecretName}
	if err := r.Client.Get(ctx, pacSecretKey, &pacSecret); err != nil {
		if !errors.IsNotFound(err) {
			r.EventRecorder.Event(&pacSecret, "Warning", "ErrorReadingPaCSecret", err.Error())
			return nil, fmt.Errorf("failed to get Pipelines as Code secret in %s namespace: %w", component.Namespace, err)
		}

		// Fallback to the global configuration
		globalPaCSecretKey := types.NamespacedName{Namespace: buildServiceNamespaceName, Name: gitopsprepare.PipelinesAsCodeSecretName}
		if err := r.Client.Get(ctx, globalPaCSecretKey, &pacSecret); err != nil {
			if !errors.IsNotFound(err) {
				r.EventRecorder.Event(&pacSecret, "Warning", "ErrorReadingPaCSecret", err.Error())
				return nil, fmt.Errorf("failed to get Pipelines as Code secret in %s namespace: %w", globalPaCSecretKey.Namespace, err)
			}

			r.EventRecorder.Event(&pacSecret, "Warning", "PaCSecretNotFound", err.Error())
			// Do not trigger a new reconcile. The PaC secret must be created first.
			return nil, boerrors.NewBuildOpError(boerrors.EPaCSecretNotFound,
				fmt.Errorf(" Pipelines as Code secret not found in %s namespace nor in %s", pacSecretKey.Namespace, globalPaCSecretKey.Namespace))
		}

		if !gitops.IsPaCApplicationConfigured(gitProvider, pacSecret.Data) {
			// Webhook is used. We need to reference access token in the component namespace.
			// Copy global PaC configuration in component namespace
			localPaCSecret := &corev1.Secret{
				TypeMeta: pacSecret.TypeMeta,
				ObjectMeta: metav1.ObjectMeta{
					Name:      pacSecretKey.Name,
					Namespace: pacSecretKey.Namespace,
					Labels: map[string]string{
						PartOfLabelName: PartOfAppStudioLabelValue,
					},
				},
				Data: pacSecret.Data,
			}
			if err := r.Client.Create(ctx, localPaCSecret); err != nil {
				return nil, fmt.Errorf("failed to create local PaC configuration secret: %w", err)
			}
		}
	}

	return &pacSecret, nil
}

// Returns webhook secret for given component.
// Generates the webhook secret and saves it the k8s secret if doesn't exist.
func (r *ComponentBuildReconciler) ensureWebhookSecret(ctx context.Context, component *appstudiov1alpha1.Component) (string, error) {
	log := ctrllog.FromContext(ctx)

	webhookSecretsSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: gitops.PipelinesAsCodeWebhooksSecretName, Namespace: component.GetNamespace()}, webhookSecretsSecret); err != nil {
		if errors.IsNotFound(err) {
			webhookSecretsSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gitops.PipelinesAsCodeWebhooksSecretName,
					Namespace: component.GetNamespace(),
					Labels: map[string]string{
						PartOfLabelName: PartOfAppStudioLabelValue,
					},
				},
			}
			if err := r.Client.Create(ctx, webhookSecretsSecret); err != nil {
				log.Error(err, "failed to create webhooks secrets secret", l.Action, l.ActionAdd)
				return "", err
			}
			return r.ensureWebhookSecret(ctx, component)
		}

		log.Error(err, "failed to get webhook secrets secret", l.Action, l.ActionView)
		return "", err
	}

	componentWebhookSecretKey := gitops.GetWebhookSecretKeyForComponent(*component)
	if _, exists := webhookSecretsSecret.Data[componentWebhookSecretKey]; exists {
		// The webhook secret already exists. Use single secret for the same repository.
		return string(webhookSecretsSecret.Data[componentWebhookSecretKey]), nil
	}

	webhookSecretString := generatePaCWebhookSecretString()

	if webhookSecretsSecret.Data == nil {
		webhookSecretsSecret.Data = make(map[string][]byte)
	}
	webhookSecretsSecret.Data[componentWebhookSecretKey] = []byte(webhookSecretString)
	if err := r.Client.Update(ctx, webhookSecretsSecret); err != nil {
		log.Error(err, "failed to update webhook secrets secret", l.Action, l.ActionUpdate)
		return "", err
	}

	return webhookSecretString, nil
}

// generatePaCWebhookSecretString generates string alike openssl rand -hex 20
func generatePaCWebhookSecretString() string {
	length := 20 // in bytes
	tokenBytes := make([]byte, length)
	if _, err := rand.Read(tokenBytes); err != nil {
		panic("Failed to read from random generator")
	}
	return hex.EncodeToString(tokenBytes)
}

// getPaCWebhookTargetUrl returns URL to which events from git repository should be sent.
func (r *ComponentBuildReconciler) getPaCWebhookTargetUrl(ctx context.Context) (string, error) {
	webhookTargetUrl := os.Getenv(pipelinesAsCodeRouteEnvVar)
	if webhookTargetUrl == "" {
		// The env variable is not set
		// Use the installed on the cluster Pipelines as Code
		var err error
		webhookTargetUrl, err = r.getPaCRoutePublicUrl(ctx)
		if err != nil {
			return "", err
		}
	}
	return webhookTargetUrl, nil
}

// getPaCRoutePublicUrl returns Pipelines as Code public route that recieves events to trigger new pipeline runs.
func (r *ComponentBuildReconciler) getPaCRoutePublicUrl(ctx context.Context) (string, error) {
	pacWebhookRoute := &routev1.Route{}
	pacWebhookRouteKey := types.NamespacedName{Namespace: pipelinesAsCodeNamespace, Name: pipelinesAsCodeRouteName}
	if err := r.Client.Get(ctx, pacWebhookRouteKey, pacWebhookRoute); err != nil {
		if !errors.IsNotFound(err) {
			return "", fmt.Errorf("failed to get Pipelines as Code route in %s namespace: %w", pacWebhookRouteKey.Namespace, err)
		}
		// Fallback to old PaC namesapce
		pacWebhookRouteKey.Namespace = pipelinesAsCodeNamespaceFallback
		if err := r.Client.Get(ctx, pacWebhookRouteKey, pacWebhookRoute); err != nil {
			if !errors.IsNotFound(err) {
				return "", fmt.Errorf("failed to get Pipelines as Code route in %s namespace: %w", pacWebhookRouteKey.Namespace, err)
			}
			// Pipelines as Code public route was not found in expected namespaces
			// Consider this error permanent
			return "", boerrors.NewBuildOpError(boerrors.EPaCRouteDoesNotExist,
				fmt.Errorf("PaC route not found in %s nor %s namespace", pipelinesAsCodeNamespace, pipelinesAsCodeNamespaceFallback))
		}
	}
	return "https://" + pacWebhookRoute.Spec.Host, nil
}

// validatePaCConfiguration detects checks that all required fields is set for whatever method is used.
func validatePaCConfiguration(gitProvider string, config map[string][]byte) error {
	isApp := gitops.IsPaCApplicationConfigured(gitProvider, config)

	expectedPaCWebhookConfigFields := []string{gitops.GetProviderTokenKey(gitProvider)}

	var err error
	switch gitProvider {
	case "github":
		if isApp {
			// GitHub application

			err = checkMandatoryFieldsNotEmpty(config, []string{gitops.PipelinesAsCode_githubAppIdKey, gitops.PipelinesAsCode_githubPrivateKey})
			if err != nil {
				break
			}

			// validate content of the fields
			if _, e := strconv.ParseInt(string(config[gitops.PipelinesAsCode_githubAppIdKey]), 10, 64); e != nil {
				err = fmt.Errorf(" Pipelines as Code: failed to parse GitHub application ID. Cause: %w", e)
				break
			}

			privateKey := strings.TrimSpace(string(config[gitops.PipelinesAsCode_githubPrivateKey]))
			if !strings.HasPrefix(privateKey, "-----BEGIN RSA PRIVATE KEY-----") ||
				!strings.HasSuffix(privateKey, "-----END RSA PRIVATE KEY-----") {
				err = fmt.Errorf(" Pipelines as Code secret: GitHub application private key is invalid")
				break
			}
		} else {
			// webhook
			err = checkMandatoryFieldsNotEmpty(config, expectedPaCWebhookConfigFields)
		}

	case "gitlab":
		err = checkMandatoryFieldsNotEmpty(config, expectedPaCWebhookConfigFields)

	case "bitbucket":
		err = checkMandatoryFieldsNotEmpty(config, []string{gitops.GetProviderTokenKey(gitProvider)})
		if err != nil {
			break
		}

		if len(config["username"]) == 0 {
			err = fmt.Errorf(" Pipelines as Code secret: name of the user field must be configured")
		}

	default:
		err = fmt.Errorf("unsupported git provider: %s", gitProvider)
	}

	return err
}

func checkMandatoryFieldsNotEmpty(config map[string][]byte, mandatoryFields []string) error {
	for _, field := range mandatoryFields {
		if len(config[field]) == 0 {
			return fmt.Errorf(" Pipelines as Code secret: %s field is not configured", field)
		}
	}
	return nil
}

func (r *ComponentBuildReconciler) ensurePaCRepository(ctx context.Context, component *appstudiov1alpha1.Component, config map[string][]byte) error {
	log := ctrllog.FromContext(ctx)

	repository, err := gitops.GeneratePACRepository(*component, config)
	if err != nil {
		return err
	}

	existingRepository := &pacv1alpha1.Repository{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: repository.Name, Namespace: repository.Namespace}, existingRepository); err != nil {
		if errors.IsNotFound(err) {
			if err := controllerutil.SetOwnerReference(component, repository, r.Scheme); err != nil {
				return err
			}
			if err := r.Client.Create(ctx, repository); err != nil {
				log.Error(err, "failed to create Component PaC repository object", l.Action, l.ActionAdd)
				return err
			}
		} else {
			log.Error(err, "failed to get Component PaC repository object", l.Action, l.ActionView)
			return err
		}
	}
	return nil
}

// generatePaCPipelineRunConfigs generates PipelineRun YAML configs for given component.
// The generated PipelineRun Yaml content are returned in byte string and in the order of push and pull request.
func (r *ComponentBuildReconciler) generatePaCPipelineRunConfigs(ctx context.Context, component *appstudiov1alpha1.Component, pacTargetBranch string) ([]byte, []byte, error) {
	log := ctrllog.FromContext(ctx)

	pipelineRef, additionalPipelineParams, err := r.GetPipelineForComponent(ctx, component)
	if err != nil {
		return nil, nil, err
	}
	log.Info(fmt.Sprintf("Selected %s pipeline from %s bundle for %s component",
		pipelineRef.Name, pipelineRef.Bundle, component.Name),
		l.Audit, "true")

	// Get pipeline from the bundle to be expanded to the PipelineRun
	pipelineSpec, err := retrievePipelineSpec(pipelineRef.Bundle, pipelineRef.Name)
	if err != nil {
		r.EventRecorder.Event(component, "Warning", "ErrorGettingPipelineFromBundle", err.Error())
		return nil, nil, err
	}

	pipelineRunOnPush, err := generatePaCPipelineRunForComponent(
		component, pipelineSpec, additionalPipelineParams, false, pacTargetBranch, log)
	if err != nil {
		return nil, nil, err
	}
	pipelineRunOnPushYaml, err := yaml.Marshal(pipelineRunOnPush)
	if err != nil {
		return nil, nil, err
	}

	pipelineRunOnPR, err := generatePaCPipelineRunForComponent(
		component, pipelineSpec, additionalPipelineParams, true, pacTargetBranch, log)
	if err != nil {
		return nil, nil, err
	}
	pipelineRunOnPRYaml, err := yaml.Marshal(pipelineRunOnPR)
	if err != nil {
		return nil, nil, err
	}

	return pipelineRunOnPushYaml, pipelineRunOnPRYaml, nil
}

func generateMergeRequestSourceBranch(component *appstudiov1alpha1.Component) string {
	return fmt.Sprintf("%s%s", pacMergeRequestSourceBranchPrefix, component.Name)
}

// ConfigureRepositoryForPaC creates a merge request with initial Pipelines as Code configuration
// and configures a webhook to notify in-cluster PaC unless application (on the repository side) is used.
func (r *ComponentBuildReconciler) ConfigureRepositoryForPaC(ctx context.Context, component *appstudiov1alpha1.Component, config map[string][]byte, webhookTargetUrl, webhookSecret string) (prUrl string, err error) {
	log := ctrllog.FromContext(ctx).WithValues("repository", component.Spec.Source.GitSource.URL)
	ctx = ctrllog.IntoContext(ctx, log)

	gitProvider, _ := gitops.GetGitProvider(*component)
	isAppUsed := gitops.IsPaCApplicationConfigured(gitProvider, config)

	var accessToken string
	if !isAppUsed {
		accessToken = strings.TrimSpace(string(config[gitops.GetProviderTokenKey(gitProvider)]))
	}

	// https://github.com/owner/repository
	gitSourceUrlParts := strings.Split(strings.TrimSuffix(component.Spec.Source.GitSource.URL, ".git"), "/")

	commitMessage := "Appstudio update " + component.Name
	branch := generateMergeRequestSourceBranch(component)
	mrTitle := "Appstudio update " + component.Name
	mrText := mergeRequestDescription
	authorName := "redhat-appstudio"
	authorEmail := "rhtap@redhat.com"

	var baseBranch string
	if component.Spec.Source.GitSource != nil {
		baseBranch = component.Spec.Source.GitSource.Revision
	}

	switch gitProvider {
	case "github":
		owner := gitSourceUrlParts[3]
		repository := gitSourceUrlParts[4]

		var ghclient *github.GithubClient
		if isAppUsed {
			githubAppIdStr := string(config[gitops.PipelinesAsCode_githubAppIdKey])
			githubAppId, err := strconv.ParseInt(githubAppIdStr, 10, 64)
			if err != nil {
				return "", fmt.Errorf("failed to convert %s to int: %w", githubAppIdStr, err)
			}

			privateKey := config[gitops.PipelinesAsCode_githubPrivateKey]
			ghclient, err = github.NewGithubClientByApp(githubAppId, privateKey, owner)
			if err != nil {
				return "", err
			}

			// Check if the application is installed into target repository
			appInstalled, err := github.IsAppInstalledIntoRepository(ghclient, owner, repository)
			if err != nil {
				return "", err
			}
			if !appInstalled {
				return "", boerrors.NewBuildOpError(boerrors.EGitHubAppNotInstalled, fmt.Errorf("GitHub Application is not installed into the repository"))
			}

			// Customize PR data to reflect GitHub App name
			if appName, appSlug, err := github.GetGitHubAppName(githubAppId, privateKey); err == nil {
				commitMessage = fmt.Sprintf("%s update %s", appName, component.Name)
				mrTitle = fmt.Sprintf("%s update %s", appName, component.Name)
				authorName = appSlug
			} else {
				log.Error(err, "failed to get GitHub Application name", l.Action, l.ActionView, l.Audit, "true")
				// Do not fail PaC provision if failed to read GitHub App info
			}

		} else {
			// Webhook
			ghclient = github.NewGithubClient(accessToken)

			err = github.SetupPaCWebhook(ghclient, webhookTargetUrl, webhookSecret, owner, repository)
			if err != nil {
				log.Error(err, fmt.Sprintf("failed to setup Pipelines as Code webhook %s", webhookTargetUrl), l.Audit, "true")
				return "", err
			} else {
				log.Info(fmt.Sprintf("Pipelines as Code webhook \"%s\" configured for %s Component in %s namespace",
					webhookTargetUrl, component.GetName(), component.GetNamespace()),
					l.Audit, "true")
			}
		}

		if baseBranch == "" {
			baseBranch, err = github.GetDefaultBranch(ghclient, owner, repository)
			if err != nil {
				return "", nil
			}
		}

		pipelineRunOnPushYaml, pipelineRunOnPRYaml, err := r.generatePaCPipelineRunConfigs(ctx, component, baseBranch)
		if err != nil {
			return "", err
		}
		prData := &github.PaCPullRequestData{
			Owner:         owner,
			Repository:    repository,
			CommitMessage: commitMessage,
			Branch:        branch,
			BaseBranch:    baseBranch,
			PRTitle:       mrTitle,
			PRText:        mrText,
			AuthorName:    authorName,
			AuthorEmail:   authorEmail,
			Files: []github.File{
				{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPushFilename, Content: pipelineRunOnPushYaml},
				{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPRFilename, Content: pipelineRunOnPRYaml},
			},
		}
		prUrl, err = github.CreatePaCPullRequest(ghclient, prData)
		if err != nil {
			// Handle case when GitHub application is not installed for the component repository
			if strings.Contains(err.Error(), "Resource not accessible by integration") {
				return "", fmt.Errorf(" Pipelines as Code GitHub application with %s ID is not installed for %s repository",
					string(config[gitops.PipelinesAsCode_githubAppIdKey]), component.Spec.Source.GitSource.URL)
			}
			return "", err
		}

		return prUrl, nil

	case "gitlab":
		glclient, err := gitlab.NewGitlabClient(accessToken)
		if err != nil {
			return "", err
		}

		gitlabNamespace := gitSourceUrlParts[3]
		gitlabProjectName := gitSourceUrlParts[4]
		projectPath := gitlabNamespace + "/" + gitlabProjectName

		err = gitlab.SetupPaCWebhook(glclient, projectPath, webhookTargetUrl, webhookSecret)
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to setup Pipelines as Code webhook %s", webhookTargetUrl), l.Audit, "true")
			return "", err
		} else {
			log.Info(fmt.Sprintf("Pipelines as Code webhook \"%s\" configured for %s Component in %s namespace",
				webhookTargetUrl, component.GetName(), component.GetNamespace()),
				l.Audit, "true")
		}

		if baseBranch == "" {
			baseBranch, err = gitlab.GetDefaultBranch(glclient, projectPath)
			if err != nil {
				return "", nil
			}
		}

		pipelineRunOnPushYaml, pipelineRunOnPRYaml, err := r.generatePaCPipelineRunConfigs(ctx, component, baseBranch)
		if err != nil {
			return "", err
		}
		mrData := &gitlab.PaCMergeRequestData{
			ProjectPath:   projectPath,
			CommitMessage: commitMessage,
			Branch:        branch,
			BaseBranch:    baseBranch,
			MrTitle:       mrTitle,
			MrText:        mrText,
			AuthorName:    authorName,
			AuthorEmail:   authorEmail,
			Files: []gitlab.File{
				{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPushFilename, Content: pipelineRunOnPushYaml},
				{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPRFilename, Content: pipelineRunOnPRYaml},
			},
		}
		mrUrl, err := gitlab.EnsurePaCMergeRequest(glclient, mrData)
		return mrUrl, err

	case "bitbucket":
		// TODO implement
		return "", fmt.Errorf("git provider %s is not supported", gitProvider)
	default:
		return "", fmt.Errorf("git provider %s is not supported", gitProvider)
	}
}

// UnconfigureRepositoryForPaC creates a merge request that deletes Pipelines as Code configuration of the diven component in its repository.
// Deletes PaC webhook if it's used.
// Does not delete PaC GitHub application from the repository as its installation was done manually by the user.
// Returns merge request web URL or empty string if it's not needed.
func (r *ComponentBuildReconciler) UnconfigureRepositoryForPaC(ctx context.Context, component *appstudiov1alpha1.Component, config map[string][]byte, webhookTargetUrl string) (prUrl string, action string, err error) {
	log := ctrllog.FromContext(ctx)

	gitProvider, _ := gitops.GetGitProvider(*component)
	isAppUsed := gitops.IsPaCApplicationConfigured(gitProvider, config)

	var accessToken string
	if !isAppUsed {
		accessToken = strings.TrimSpace(string(config[gitops.GetProviderTokenKey(gitProvider)]))
	}

	// https://github.com/owner/repository
	gitSourceUrlParts := strings.Split(strings.TrimSuffix(component.Spec.Source.GitSource.URL, ".git"), "/")

	commitMessage := "Appstudio purge " + component.Name
	branch := "appstudio-purge-" + component.Name
	mrTitle := "Appstudio purge " + component.Name
	mrText := "Pipelines as Code configuration removal"
	authorName := "redhat-appstudio"
	authorEmail := "appstudio@redhat.com"

	var baseBranch string
	if component.Spec.Source.GitSource != nil {
		baseBranch = component.Spec.Source.GitSource.Revision
	}

	switch gitProvider {
	case "github":
		owner := gitSourceUrlParts[3]
		repository := gitSourceUrlParts[4]

		var ghclient *github.GithubClient
		if isAppUsed {
			githubAppIdStr := string(config[gitops.PipelinesAsCode_githubAppIdKey])
			githubAppId, err := strconv.ParseInt(githubAppIdStr, 10, 64)
			if err != nil {
				return "", "", fmt.Errorf("failed to convert %s to int: %w", githubAppIdStr, err)
			}

			privateKey := config[gitops.PipelinesAsCode_githubPrivateKey]
			ghclient, err = github.NewGithubClientByApp(githubAppId, privateKey, owner)
			if err != nil {
				return "", "", err
			}
		} else {
			// Webhook
			ghclient = github.NewGithubClient(accessToken)

			if webhookTargetUrl != "" {
				err = github.DeletePaCWebhook(ghclient, webhookTargetUrl, owner, repository)
				if err != nil {
					// Just log the error and continue with merge request creation
					log.Error(err, fmt.Sprintf("failed to delete Pipelines as Code webhook %s", webhookTargetUrl), l.Action, l.ActionDelete, l.Audit, "true")
				} else {
					log.Info(fmt.Sprintf("Pipelines as Code webhook \"%s\" deleted for %s Component in %s namespace",
						webhookTargetUrl, component.GetName(), component.GetNamespace()),
						l.Action, l.ActionDelete)
				}
			}
		}

		if baseBranch == "" {
			baseBranch, err = github.GetDefaultBranch(ghclient, owner, repository)
			if err != nil {
				return "", "", nil
			}
		}

		sourceBranch := generateMergeRequestSourceBranch(component)
		pullRequest, err := github.FindUnmergedOnboardingMergeRequest(ghclient, owner, repository, sourceBranch, baseBranch, owner)
		if err != nil {
			return "", "", err
		}

		if pullRequest == nil {
			prData := &github.PaCPullRequestData{
				Owner:         owner,
				Repository:    repository,
				CommitMessage: commitMessage,
				Branch:        branch,
				BaseBranch:    baseBranch,
				PRTitle:       mrTitle,
				PRText:        mrText,
				AuthorName:    authorName,
				AuthorEmail:   authorEmail,
				Files: []github.File{
					{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPushFilename},
					{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPRFilename},
				},
			}
			prUrl, err = github.UndoPaCPullRequest(ghclient, prData)
			if err != nil {
				// Handle case when GitHub application is not installed for the component repository
				if strings.Contains(err.Error(), "Resource not accessible by integration") {
					return "", "", fmt.Errorf(" Pipelines as Code GitHub application with %s ID is not installed for %s repository",
						string(config[gitops.PipelinesAsCode_githubAppIdKey]), component.Spec.Source.GitSource.URL)
				}
				return "", "", err
			}
			return prUrl, "delete", nil
		} else {
			err := github.DeleteBranch(ghclient, owner, repository, sourceBranch)
			if err == nil {
				log.Info(fmt.Sprintf("pull request source branch %s is deleted", sourceBranch), l.Action, l.ActionDelete)
				return prUrl, "close", nil
			}
			// Non-existing source branch should not be an error, just ignore it
			// but other errors should be handled.
			if ghErrResp, ok := err.(*gogithub.ErrorResponse); ok {
				if ghErrResp.Response.StatusCode == 422 {
					log.Info(fmt.Sprintf("Tried to delete source branch %s, but it does not exist in the repository", sourceBranch))
					return prUrl, "close", nil
				}
			}
			return "", "", err
		}

	case "gitlab":
		glclient, err := gitlab.NewGitlabClient(accessToken)
		if err != nil {
			return "", "", err
		}

		gitlabNamespace := gitSourceUrlParts[3]
		gitlabProjectName := gitSourceUrlParts[4]
		projectPath := gitlabNamespace + "/" + gitlabProjectName

		err = gitlab.DeletePaCWebhook(glclient, projectPath, webhookTargetUrl)
		if err != nil {
			// Just log the error and continue with merge request creation
			log.Error(err, "failed to delete Pipelines as Code webhook", l.Action, l.ActionDelete, l.Audit, "true")
		}

		if baseBranch == "" {
			baseBranch, err = gitlab.GetDefaultBranch(glclient, projectPath)
			if err != nil {
				return "", "", nil
			}
		}

		sourceBranch := generateMergeRequestSourceBranch(component)
		mr, err := gitlab.FindUnmergedOnboardingMergeRequest(glclient, projectPath, sourceBranch, baseBranch, authorName)
		if err != nil {
			return "", "", err
		}

		if mr == nil {
			mrData := &gitlab.PaCMergeRequestData{
				ProjectPath:   projectPath,
				CommitMessage: commitMessage,
				Branch:        branch,
				BaseBranch:    baseBranch,
				MrTitle:       mrTitle,
				MrText:        mrText,
				AuthorName:    authorName,
				AuthorEmail:   authorEmail,
				Files: []gitlab.File{
					{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPushFilename},
					{FullPath: ".tekton/" + component.Name + "-" + pipelineRunOnPRFilename},
				},
			}
			mrUrl, err := gitlab.UndoPaCMergeRequest(glclient, mrData)
			if err != nil {
				return "", "", err
			}
			return mrUrl, "delete", nil
		} else {
			err := gitlab.DeleteBranch(glclient, projectPath, sourceBranch)
			if err == nil {
				log.Info(fmt.Sprintf("merge request source branch %s is deleted", sourceBranch), l.Action, l.ActionDelete)
				return mr.WebURL, "close", nil
			}
			if glErrResp, ok := err.(*gogitlab.ErrorResponse); ok {
				if glErrResp.Response.StatusCode == 404 {
					log.Info(fmt.Sprintf("Tried to delete source branch %s, but it does not exist in repository", sourceBranch))
					return mr.WebURL, "close", nil
				}
			}
			return "", "", err
		}

	case "bitbucket":
		// TODO implement
		return "", "", fmt.Errorf("git provider %s is not supported", gitProvider)
	default:
		return "", "", fmt.Errorf("git provider %s is not supported", gitProvider)
	}
}

// generatePaCPipelineRunForComponent returns pipeline run definition to build component source with.
// Generated pipeline run contains placeholders that are expanded by Pipeline-as-Code.
func generatePaCPipelineRunForComponent(
	component *appstudiov1alpha1.Component,
	pipelineSpec *tektonapi.PipelineSpec,
	additionalPipelineParams []tektonapi.Param,
	onPull bool,
	pacTargetBranch string,
	log logr.Logger) (*tektonapi.PipelineRun, error) {

	if pacTargetBranch == "" {
		return nil, fmt.Errorf("target branch can't be empty for generating PaC PipelineRun for: %v", component)
	}

	annotations := map[string]string{
		"pipelinesascode.tekton.dev/on-target-branch": "[" + pacTargetBranch + "]",
		"pipelinesascode.tekton.dev/max-keep-runs":    "3",
		"build.appstudio.redhat.com/target_branch":    "{{target_branch}}",
		gitCommitShaAnnotationName:                    "{{revision}}",
	}
	labels := map[string]string{
		ApplicationNameLabelName:                component.Spec.Application,
		ComponentNameLabelName:                  component.Name,
		"pipelines.appstudio.openshift.io/type": "build",
	}

	var gitRepoAtShaUrl string
	gitProvider, _ := gitops.GetGitProvider(*component)
	switch gitProvider {
	case "github":
		gitRepoAtShaUrl = github.GetBrowseRepositoryAtShaLink(component.Spec.Source.GitSource.URL, "{{revision}}")
	case "gitlab":
		gitRepoAtShaUrl = gitlab.GetBrowseRepositoryAtShaLink(component.Spec.Source.GitSource.URL, "{{revision}}")
	}
	if gitRepoAtShaUrl != "" {
		annotations[gitRepoAtShaAnnotationName] = gitRepoAtShaUrl
	}

	imageRepo := getContainerImageRepositoryForComponent(component)

	var pipelineName string
	var proposedImage string
	if onPull {
		annotations["pipelinesascode.tekton.dev/on-event"] = "[pull_request]"
		annotations["build.appstudio.redhat.com/pull_request_number"] = "{{pull_request_number}}"
		pipelineName = component.Name + pipelineRunOnPRSuffix
		proposedImage = imageRepo + ":on-pr-{{revision}}"
	} else {
		annotations["pipelinesascode.tekton.dev/on-event"] = "[push]"
		pipelineName = component.Name + pipelineRunOnPushSuffix
		proposedImage = imageRepo + ":{{revision}}"
	}

	params := []tektonapi.Param{
		{Name: "git-url", Value: tektonapi.ArrayOrString{Type: "string", StringVal: "{{repo_url}}"}},
		{Name: "revision", Value: tektonapi.ArrayOrString{Type: "string", StringVal: "{{revision}}"}},
		{Name: "output-image", Value: tektonapi.ArrayOrString{Type: "string", StringVal: proposedImage}},
	}
	if onPull {
		expiration := os.Getenv(pipelineRunOnPRExpirationEnvVar)
		validExpiration, _ := regexp.Match("^[1-9][0-9]{0,2}[hdw]$", []byte(expiration))
		if !validExpiration {
			if expiration != "" {
				log.Info(fmt.Sprintf("invalid expiration '%s' in %s envVar, using default %s", expiration, pipelineRunOnPRExpirationEnvVar, pipelineRunOnPRExpirationDefault), l.Action, l.ActionAdd)
			}
			expiration = pipelineRunOnPRExpirationDefault
		}
		params = append(params, tektonapi.Param{Name: "image-expires-after", Value: tektonapi.ArrayOrString{Type: "string", StringVal: expiration}})
	}

	dockerFile, err := devfile.SearchForDockerfile([]byte(component.Status.Devfile))
	if err != nil {
		return nil, err
	}
	if dockerFile != nil {
		if dockerFile.Uri != "" {
			params = append(params, tektonapi.Param{Name: "dockerfile", Value: tektonapi.ArrayOrString{Type: "string", StringVal: dockerFile.Uri}})
		}
		pathContext := getPathContext(component.Spec.Source.GitSource.Context, dockerFile.BuildContext)
		if pathContext != "" {
			params = append(params, tektonapi.Param{Name: "path-context", Value: tektonapi.ArrayOrString{Type: "string", StringVal: pathContext}})
		}
	}

	params = mergeAndSortTektonParams(params, additionalPipelineParams)

	pipelineRunWorkspaces := createWorkspaceBinding(pipelineSpec.Workspaces)

	pipelineRun := &tektonapi.PipelineRun{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PipelineRun",
			APIVersion: "tekton.dev/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        pipelineName,
			Namespace:   component.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: tektonapi.PipelineRunSpec{
			PipelineSpec: pipelineSpec,
			Params:       params,
			Workspaces:   pipelineRunWorkspaces,
		},
	}

	return pipelineRun, nil
}

func createWorkspaceBinding(pipelineWorkspaces []tektonapi.PipelineWorkspaceDeclaration) []tektonapi.WorkspaceBinding {
	pipelineRunWorkspaces := []tektonapi.WorkspaceBinding{}
	for _, workspace := range pipelineWorkspaces {
		switch workspace.Name {
		case "workspace":
			pipelineRunWorkspaces = append(pipelineRunWorkspaces,
				tektonapi.WorkspaceBinding{
					Name:                workspace.Name,
					VolumeClaimTemplate: generateVolumeClaimTemplate(),
				})
		case "git-auth":
			pipelineRunWorkspaces = append(pipelineRunWorkspaces,
				tektonapi.WorkspaceBinding{
					Name:   workspace.Name,
					Secret: &corev1.SecretVolumeSource{SecretName: "{{ git_auth_secret }}"},
				})
		}
	}
	return pipelineRunWorkspaces
}

// retrievePipelineSpec retrieves pipeline definition with given name from the given bundle.
func retrievePipelineSpec(bundleUri, pipelineName string) (*tektonapi.PipelineSpec, error) {
	var obj runtime.Object
	var err error
	resolver := oci.NewResolver(bundleUri, authn.DefaultKeychain)

	if obj, _, err = resolver.Get(context.TODO(), "pipeline", pipelineName); err != nil {
		return nil, err
	}
	pipelineSpecObj, ok := obj.(tektonapi.PipelineObject)
	if !ok {
		return nil, fmt.Errorf("failed to extract pipeline %s from bundle %s", bundleUri, pipelineName)
	}
	pipelineSpec := pipelineSpecObj.PipelineSpec()
	return &pipelineSpec, nil
}
