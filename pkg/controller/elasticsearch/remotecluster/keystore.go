// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package remotecluster

import (
	"context"
	"encoding/json"
	"fmt"
	commonv1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/common/v1"
	"github.com/elastic/cloud-on-k8s/v2/pkg/controller/common/keystore"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	esv1 "github.com/elastic/cloud-on-k8s/v2/pkg/apis/elasticsearch/v1"
	"github.com/elastic/cloud-on-k8s/v2/pkg/controller/common/labels"
	"github.com/elastic/cloud-on-k8s/v2/pkg/controller/common/reconciler"
	"github.com/elastic/cloud-on-k8s/v2/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/v2/pkg/utils/k8s"
	ulog "github.com/elastic/cloud-on-k8s/v2/pkg/utils/log"
)

const (
	aliasesAnnotationName = "elasticsearch.k8s.elastic.co/remote-clusters-keys"
)

var (
	credentialsSecretSettingsRegEx = regexp.MustCompile(`^cluster\.remote\.([\w-]+)\.credentials$`)
)

type APIKeyStore struct {
	// aliases maps cluster aliased with the expected key ID
	aliases map[string]string
	// keys maps the ID of an API Key (not its name), to the encoded cross-cluster API key.
	keys map[string]string
}

func (aks *APIKeyStore) KeyIDFor(alias string) string {
	if aks == nil {
		return ""
	}
	return aks.aliases[alias]
}

func LoadAPIKeyStore(ctx context.Context, c k8s.Client, owner *esv1.Elasticsearch) (*APIKeyStore, error) {
	secretName := types.NamespacedName{
		Name:      esv1.RemoteAPIKeysSecretName(owner.Name),
		Namespace: owner.Namespace,
	}
	// Attempt to read the Secret
	keyStoreSecret := &corev1.Secret{}
	if err := c.Get(ctx, secretName, keyStoreSecret); err != nil {
		if errors.IsNotFound(err) {
			ulog.FromContext(ctx).V(1).Info("No APIKeyStore Secret found",
				"namespace", owner.Namespace,
				"es_name", owner.Name,
			)
			// Return an empty store
			return &APIKeyStore{}, nil
		}
	}

	// Read the key aliased
	aliases := make(map[string]string)
	if aliasesAnnotation, ok := keyStoreSecret.Annotations[aliasesAnnotationName]; ok {
		if err := json.Unmarshal([]byte(aliasesAnnotation), &aliases); err != nil {
			return nil, err
		}
	}

	// Read the current encoded cross-cluster API keys.
	keys := make(map[string]string)
	for settingName, encodedAPIKey := range keyStoreSecret.Data {
		strings := credentialsSecretSettingsRegEx.FindStringSubmatch(settingName)
		if len(strings) != 2 {
			ulog.FromContext(ctx).V(1).Info(
				fmt.Sprintf("Unknown remote cluster credential setting: %s", settingName),
				"namespace", owner.Namespace,
				"es_name", owner.Name,
			)
			continue
		}
		keys[strings[1]] = string(encodedAPIKey)
	}
	return &APIKeyStore{
		aliases: aliases,
		keys:    keys,
	}, nil
}

func (aks *APIKeyStore) Update(alias, keyID, encodedKeyValue string) *APIKeyStore {
	if aks.aliases == nil {
		aks.aliases = make(map[string]string)
	}
	aks.aliases[alias] = keyID
	if aks.keys == nil {
		aks.keys = make(map[string]string)
	}
	aks.keys[alias] = encodedKeyValue
	return aks
}

func (aks *APIKeyStore) Delete(alias string) *APIKeyStore {
	delete(aks.aliases, alias)
	delete(aks.keys, alias)
	return aks
}

const (
	credentialsKeyFormat = "cluster.remote.%s.credentials"
)

func (aks *APIKeyStore) Save(ctx context.Context, c k8s.Client, owner *esv1.Elasticsearch) error {
	secretName := types.NamespacedName{
		Name:      esv1.RemoteAPIKeysSecretName(owner.Name),
		Namespace: owner.Namespace,
	}
	if aks.IsEmpty() {
		// Check if the Secret does exist.
		currentSecret := corev1.Secret{}
		if err := c.Get(ctx, secretName, &currentSecret); err != nil {
			if errors.IsNotFound(err) {
				// Secret does not exist.
				return nil
			}
			return err
		}
		// Delete the Secret we just detected above.
		if err := c.Delete(ctx,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName.Name, Namespace: secretName.Namespace}},
			&client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &currentSecret.UID}},
		); err != nil {
			return err
		}
		return nil
	}

	aliases, err := json.Marshal(aks.aliases)
	if err != nil {
		return err
	}
	data := make(map[string][]byte, len(aks.keys))
	for k, v := range aks.keys {
		data[fmt.Sprintf(credentialsKeyFormat, k)] = []byte(v)
	}
	expected := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName.Name,
			Namespace: secretName.Namespace,
			Annotations: map[string]string{
				aliasesAnnotationName: string(aliases),
			},
			Labels: labels.AddCredentialsLabel(label.NewLabels(k8s.ExtractNamespacedName(owner))),
		},
		Data: data,
	}
	if _, err := reconciler.ReconcileSecret(ctx, c, expected, owner); err != nil {
		return err
	}
	return nil
}

func (aks *APIKeyStore) IsEmpty() bool {
	if aks == nil {
		return true
	}
	return len(aks.aliases) == 0
}

func WithRemoteClusterAPIKeys(ctx context.Context, es *esv1.Elasticsearch, c k8s.Client) (keystore.HasKeystore, error) {
	extendedKeystore := &ExtendedKeystore{
		Elasticsearch:  es,
		secureSettings: es.SecureSettings(),
	}
	// Check if Secret exists
	secretName := types.NamespacedName{
		Name:      esv1.RemoteAPIKeysSecretName(es.Name),
		Namespace: es.Namespace,
	}
	if err := c.Get(ctx, secretName, &corev1.Secret{}); err != nil {
		if errors.IsNotFound(err) {
			return extendedKeystore, nil
		}
		return nil, err
	}
	// Add the Secret that holds the API Keys
	extendedKeystore.secureSettings = append(extendedKeystore.secureSettings, commonv1.SecretSource{SecretName: secretName.Name})
	return extendedKeystore, nil
}

type ExtendedKeystore struct {
	*esv1.Elasticsearch
	secureSettings []commonv1.SecretSource
}

func (eks *ExtendedKeystore) SecureSettings() []commonv1.SecretSource {
	if eks == nil {
		return nil
	}
	return eks.secureSettings
}

var _ keystore.HasKeystore = &ExtendedKeystore{}
