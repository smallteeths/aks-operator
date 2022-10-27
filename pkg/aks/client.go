package aks

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/containerservice/mgmt/2020-11-01/containerservice"
	"github.com/Azure/azure-sdk-for-go/services/operationalinsights/mgmt/2020-08-01/operationalinsights"
	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2019-10-01/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	aksv1 "github.com/rancher/aks-operator/pkg/apis/aks.cattle.io/v1"
	"github.com/rancher/aks-operator/pkg/utils"
	"github.com/rancher/machine/drivers/azure/azureutil"
	wranglerv1 "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
)

const (
	tenantIDAnnotation          = "cluster.management.cattle.io/azure-tenant-id"
	tenantIDTimestampAnnotation = "cluster.management.cattle.io/azure-tenant-id-created-at"
	tenantIDTimeout             = time.Hour
)

type Credentials struct {
	AuthBaseURL    *string
	BaseURL        *string
	SubscriptionID string
	TenantID       string
	ClientID       string
	ClientSecret   string
}

func NewResourceGroupClient(cred *Credentials) (*resources.GroupsClient, error) {
	authorizer, err := NewClientAuthorizer(cred)
	if err != nil {
		return nil, err
	}

	client := resources.NewGroupsClientWithBaseURI(to.String(cred.BaseURL), cred.SubscriptionID)
	client.Authorizer = authorizer

	return &client, nil
}

func NewClusterClient(cred *Credentials) (*containerservice.ManagedClustersClient, error) {
	authorizer, err := NewClientAuthorizer(cred)
	if err != nil {
		return nil, err
	}

	client := containerservice.NewManagedClustersClientWithBaseURI(to.String(cred.BaseURL), cred.SubscriptionID)
	client.Authorizer = authorizer

	return &client, nil
}

func NewAgentPoolClient(cred *Credentials) (*containerservice.AgentPoolsClient, error) {
	authorizer, err := NewClientAuthorizer(cred)
	if err != nil {
		return nil, err
	}

	agentProfile := containerservice.NewAgentPoolsClientWithBaseURI(to.String(cred.BaseURL), cred.SubscriptionID)
	agentProfile.Authorizer = authorizer

	return &agentProfile, nil
}

func NewOperationInsightsWorkspaceClient(cred *Credentials) (*operationalinsights.WorkspacesClient, error) {
	authorizer, err := NewClientAuthorizer(cred)
	if err != nil {
		return nil, err
	}

	client := operationalinsights.NewWorkspacesClientWithBaseURI(to.String(cred.BaseURL), cred.SubscriptionID)
	client.Authorizer = authorizer

	return &client, nil
}

func NewClientAuthorizer(cred *Credentials) (autorest.Authorizer, error) {
	if cred.AuthBaseURL == nil {
		cred.AuthBaseURL = to.StringPtr(azure.PublicCloud.ActiveDirectoryEndpoint)
	}

	if cred.BaseURL == nil {
		cred.BaseURL = to.StringPtr(azure.PublicCloud.ResourceManagerEndpoint)
	}

	oauthConfig, err := adal.NewOAuthConfig(to.String(cred.AuthBaseURL), cred.TenantID)
	if err != nil {
		return nil, err
	}

	spToken, err := adal.NewServicePrincipalToken(*oauthConfig, cred.ClientID, cred.ClientSecret, to.String(cred.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("couldn't authenticate to Azure cloud with error: %v", err)
	}

	return autorest.NewBearerAuthorizer(spToken), nil
}

func GetSecrets(secretsCache wranglerv1.SecretCache, secretClient wranglerv1.SecretClient, spec *aksv1.AKSClusterConfigSpec) (*Credentials, error) {
	var cred Credentials

	if spec.AzureCredentialSecret == "" {
		return nil, fmt.Errorf("secret name not provided")
	}

	ns, id := utils.ParseSecretName(spec.AzureCredentialSecret)
	secret, err := secretsCache.Get(ns, id)
	if err != nil {
		return nil, fmt.Errorf("couldn't find secret [%s] in namespace [%s]", id, ns)
	}

	tenantIDBytes := secret.Data["azurecredentialConfig-tenantId"]
	subscriptionIDBytes := secret.Data["azurecredentialConfig-subscriptionId"]
	clientIDBytes := secret.Data["azurecredentialConfig-clientId"]
	clientSecretBytes := secret.Data["azurecredentialConfig-clientSecret"]
	clientEnvironment := string(secret.Data["azurecredentialConfig-environment"])
	azureEnvironment := GetEnvironment(clientEnvironment)

	cannotBeNilError := "field [azurecredentialConfig-%s] must be provided in cloud credential"
	if subscriptionIDBytes == nil {
		return nil, fmt.Errorf(cannotBeNilError, "subscriptionId")
	}
	if clientIDBytes == nil {
		return nil, fmt.Errorf(cannotBeNilError, "clientId")
	}
	if clientSecretBytes == nil {
		return nil, fmt.Errorf(cannotBeNilError, "clientSecret")
	}

	cred.TenantID = string(tenantIDBytes)
	cred.SubscriptionID = string(subscriptionIDBytes)
	cred.ClientID = string(clientIDBytes)
	cred.ClientSecret = string(clientSecretBytes)
	cred.AuthBaseURL = &azureEnvironment.ActiveDirectoryEndpoint
	cred.BaseURL = &azureEnvironment.ResourceManagerEndpoint

	if cred.TenantID == "" {
		cred.TenantID, err = GetCachedTenantID(secretClient, cred.SubscriptionID, secret, azureEnvironment)
		if err != nil {
			return nil, err
		}
	}

	return &cred, nil
}

type secretClient interface {
	Update(*v1.Secret) (*v1.Secret, error)
}

func GetCachedTenantID(secretClient secretClient, subscriptionID string, secret *v1.Secret, azureEnvironment azure.Environment) (string, error) {
	annotations := secret.GetAnnotations()
	tenantAnno, timestamp := annotations[tenantIDAnnotation], annotations[tenantIDTimestampAnnotation]
	if tenantAnno != "" && timestamp != "" {
		parsedTime, err := time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return "", err
		}
		if parsedTime.Add(tenantIDTimeout).After(time.Now().UTC()) {
			return tenantAnno, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logrus.Debugf("retrieving tenant ID from Azure public cloud")
	tenantID, err := azureutil.FindTenantID(ctx, azureEnvironment, subscriptionID)
	if err != nil {
		return "", err
	}
	secret.Annotations[tenantIDAnnotation] = tenantID
	secret.Annotations[tenantIDTimestampAnnotation] = time.Now().UTC().Format(time.RFC3339)
	_, err = secretClient.Update(secret)
	if errors.IsConflict(err) {
		// Ignore errors when updating the secret object. If the secret cannot be updated
		// (perhaps due to a conflict error), the tenant ID will be re-fetched on the next reconcile loop.
		logrus.Debugf("encountered error while updating secret, ignoring: %v", err)
		return tenantID, nil
	}
	return tenantID, err
}

func GetEnvironment(env string) azure.Environment {
	switch env {
	case "AzureGermanCloud":
		return azure.GermanCloud
	case "AzureChinaCloud":
		return azure.ChinaCloud
	case "AzureUSGovernmentCloud":
		return azure.USGovernmentCloud
	default:
		return azure.PublicCloud
	}
}
