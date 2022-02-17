/*
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
package parameterstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/tidwall/gjson"
	ctrl "sigs.k8s.io/controller-runtime"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	"github.com/external-secrets/external-secrets/pkg/find"
	"github.com/external-secrets/external-secrets/pkg/provider/aws/util"
	utilpointer "k8s.io/utils/pointer"
)

// ParameterStore is a provider for AWS ParameterStore.
type ParameterStore struct {
	sess   *session.Session
	client PMInterface
}

// PMInterface is a subset of the parameterstore api.
// see: https://docs.aws.amazon.com/sdk-for-go/api/service/ssm/ssmiface/
type PMInterface interface {
	GetParameter(*ssm.GetParameterInput) (*ssm.GetParameterOutput, error)
	DescribeParameters(*ssm.DescribeParametersInput) (*ssm.DescribeParametersOutput, error)
}

const (
	errUnexpectedFindOperator = "unexpected find operator"
	errDuplicateKey           = "duplicate key mapping at %s"
)

var log = ctrl.Log.WithName("provider").WithName("aws").WithName("parameterstore")

// New constructs a ParameterStore Provider that is specific to a store.
func New(sess *session.Session) (*ParameterStore, error) {
	return &ParameterStore{
		sess:   sess,
		client: ssm.New(sess),
	}, nil
}

// Empty GetAllSecrets.
func (pm *ParameterStore) GetAllSecrets(ctx context.Context, ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	if ref.Name != nil {
		return pm.findByName(ref)
	}
	if len(ref.Tags) > 0 {
		return pm.findByTags(ref)
	}
	return nil, errors.New(errUnexpectedFindOperator)
}

func (pm *ParameterStore) findByName(ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	matcher, err := find.New(*ref.Name)
	if err != nil {
		return nil, err
	}
	data := make(map[string][]byte)
	var nextToken *string
	for {
		it, err := pm.client.DescribeParameters(&ssm.DescribeParametersInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		log.Info("aws pm findByName", "parameters", len(it.Parameters))
		for _, param := range it.Parameters {
			if !matcher.MatchName(*param.Name) {
				continue
			}
			err = pm.fetchAndSet(data, *param.Name)
			if err != nil {
				return nil, err
			}
		}
		nextToken = it.NextToken
		if nextToken == nil {
			break
		}
	}
	return data, nil
}

func (pm *ParameterStore) findByTags(ref esv1beta1.ExternalSecretFind) (map[string][]byte, error) {
	filters := make([]*ssm.ParameterStringFilter, len(ref.Tags))
	for k, v := range ref.Tags {
		filters = append(filters, &ssm.ParameterStringFilter{
			Key:    utilpointer.StringPtr(fmt.Sprintf("tag:%s", k)),
			Values: []*string{utilpointer.StringPtr(v)},
			Option: utilpointer.StringPtr("Equals"),
		})
	}

	data := make(map[string][]byte)
	var nextToken *string
	for {
		it, err := pm.client.DescribeParameters(&ssm.DescribeParametersInput{
			ParameterFilters: filters,
			NextToken:        nextToken,
		})
		if err != nil {
			return nil, err
		}
		log.V(1).Info("aws pm findByTags found", "parameters", len(it.Parameters))
		for _, param := range it.Parameters {
			err = pm.fetchAndSet(data, *param.Name)
			if err != nil {
				return nil, err
			}
		}
		nextToken = it.NextToken
		if nextToken == nil {
			break
		}
	}
	return data, nil
}

func (pm *ParameterStore) fetchAndSet(data map[string][]byte, name string) error {
	out, err := pm.client.GetParameter(&ssm.GetParameterInput{
		Name:           utilpointer.StringPtr(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return util.SanitizeErr(err)
	}

	// Note: multiple key names CAN collide: `/dev/my_db` and `/dev/my/db` would result
	//       in the same key `dev_my_db` being mapped
	key := mapSecretKey(name)
	if _, exists := data[key]; exists {
		return fmt.Errorf(errDuplicateKey, key)
	}

	// secret keys must consist of alphanumeric characters or `-`, `_` or `.`
	data[mapSecretKey(name)] = []byte(*out.Parameter.Value)
	return nil
}

// mapSecretKey maps the parameter key to a secret key. Example: `/foo/bar/baz` -> `foo_bar_baz`.
// The secret keys must consist of alphanumeric characters or `-`, `_` or `.`
// AWS Parameter Names use the same character set BUT in addition to that the slash character `/`
// is used to delineate hierarchies in parameter names
// see aws docs: https://docs.aws.amazon.com/systems-manager/latest/userguide/sysman-paramstore-su-create.html
// Example Parameter:  /dev/myapp/password
// Example Secret Key: dev_myapp_password.
func mapSecretKey(str string) string {
	str = strings.TrimLeft(str, "/")
	return strings.ReplaceAll(str, "/", "_")
}

// GetSecret returns a single secret from the provider.
func (pm *ParameterStore) GetSecret(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) ([]byte, error) {
	log.Info("fetching secret value", "key", ref.Key)
	out, err := pm.client.GetParameter(&ssm.GetParameterInput{
		Name:           &ref.Key,
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, util.SanitizeErr(err)
	}
	if ref.Property == "" {
		if out.Parameter.Value != nil {
			return []byte(*out.Parameter.Value), nil
		}
		return nil, fmt.Errorf("invalid secret received. parameter value is nil for key: %s", ref.Key)
	}
	val := gjson.Get(*out.Parameter.Value, ref.Property)
	if !val.Exists() {
		return nil, fmt.Errorf("key %s does not exist in secret %s", ref.Property, ref.Key)
	}
	return []byte(val.String()), nil
}

// GetSecretMap returns multiple k/v pairs from the provider.
func (pm *ParameterStore) GetSecretMap(ctx context.Context, ref esv1beta1.ExternalSecretDataRemoteRef) (map[string][]byte, error) {
	log.Info("fetching secret map", "key", ref.Key)
	data, err := pm.GetSecret(ctx, ref)
	if err != nil {
		return nil, err
	}
	kv := make(map[string]string)
	err = json.Unmarshal(data, &kv)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal secret %s: %w", ref.Key, err)
	}
	secretData := make(map[string][]byte)
	for k, v := range kv {
		secretData[k] = []byte(v)
	}
	return secretData, nil
}

func (pm *ParameterStore) Close(ctx context.Context) error {
	return nil
}

func (pm *ParameterStore) Validate() error {
	_, err := pm.sess.Config.Credentials.Get()
	return err
}
