/*
Copyright 2021 The Dapr Authors
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

package cosmosdbbinding_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/dapr/components-contrib/bindings"
	cosmosdbbinding "github.com/dapr/components-contrib/bindings/azure/cosmosdb"
	"github.com/dapr/components-contrib/secretstores"
	secretstore_env "github.com/dapr/components-contrib/secretstores/local/env"
	bindings_loader "github.com/dapr/dapr/pkg/components/bindings"
	secretstores_loader "github.com/dapr/dapr/pkg/components/secretstores"
	"github.com/dapr/dapr/pkg/runtime"
	dapr_testing "github.com/dapr/dapr/pkg/testing"
	daprsdk "github.com/dapr/go-sdk/client"
	"github.com/dapr/kit/logger"

	"github.com/dapr/components-contrib/tests/certification/embedded"
	"github.com/dapr/components-contrib/tests/certification/flow"
	"github.com/dapr/components-contrib/tests/certification/flow/sidecar"

	"github.com/a8m/documentdb"
)

const (
	sidecarName = "cosmosdb-sidecar"
)

func createDocument(generateID bool, includePK bool) map[string]interface{} {
	document := map[string]interface{}{
		"orderid": "123abc456def",
		"nestedproperty": map[string]interface{}{
			"subproperty": "something of value for testing",
		},
		"description": "certification test item",
	}
	if generateID {
		randomID := uuid.New().String()
		document["id"] = randomID
	}
	if includePK {
		document["partitionKey"] = "partitioniningOnThisValue"
	}

	return document
}

func TestCosmosDBBinding(t *testing.T) {
	ports, err := dapr_testing.GetFreePorts(2)
	assert.NoError(t, err)

	currentGRPCPort := ports[0]
	currentHTTPPort := ports[1]

	log := logger.NewLogger("dapr.components")

	invokeCreateWithDocument := func(ctx flow.Context, document map[string]interface{}) error {
		client, clientErr := daprsdk.NewClientWithPort(fmt.Sprint(currentGRPCPort))
		if clientErr != nil {
			panic(clientErr)
		}
		defer client.Close()

		bytesDoc, marshalErr := json.Marshal(document)
		if marshalErr != nil {
			return marshalErr
		}

		invokeRequest := &daprsdk.InvokeBindingRequest{
			Name:      "azure-cosmosdb-binding",
			Operation: "create",
			Data:      bytesDoc,
			Metadata:  nil,
		}

		err = client.InvokeOutputBinding(ctx, invokeRequest)
		return err
	}

	testInvokeCreateAndVerify := func(ctx flow.Context) error {
		document := createDocument(true, true)
		invokeErr := invokeCreateWithDocument(ctx, document)
		assert.NoError(t, invokeErr)

		// sleep to avoid metdata request rate limit before initializing new client
		flow.Sleep(3 * time.Second)

		// all environment variables loaded here are also loaded in the component definition YAML files
		// these are generated by the setup-azure-conf-test.sh script and injected by the GitHub Workflow, or by
		// locally sourcing the generated .rc file
		config := documentdb.NewConfig(&documentdb.Key{
			Key: os.Getenv("AzureCosmosDBMasterKey"),
		})
		config.IdentificationHydrator = nil
		dbclient := documentdb.New(os.Getenv("AzureCosmosDBUrl"), config)

		dbs, queryDBErr := dbclient.QueryDatabases(&documentdb.Query{
			Query: "SELECT * FROM ROOT r WHERE r.id=@id",
			Parameters: []documentdb.Parameter{
				{Name: "@id", Value: os.Getenv("AzureCosmosDB")},
			},
		})
		assert.NoError(t, queryDBErr)
		db := &dbs[0]
		colls, queryCollErr := dbclient.QueryCollections(db.Self, &documentdb.Query{
			Query: "SELECT * FROM ROOT r WHERE r.id=@id",
			Parameters: []documentdb.Parameter{
				{Name: "@id", Value: os.Getenv("AzureCosmosDBCollection")},
			},
		})
		assert.NoError(t, queryCollErr)
		collection := &colls[0]

		var items []map[string]interface{}
		_, queryErr := dbclient.QueryDocuments(
			collection.Self,
			documentdb.NewQuery("SELECT * FROM ROOT r WHERE r.id=@id", documentdb.P{Name: "@id", Value: document["id"].(string)}),
			&items,
			documentdb.CrossPartition(),
		)

		assert.NoError(t, queryErr)

		result := items[0]
		// verify the item retrieved from the database matches the item we inserted
		assert.Equal(t, document["id"], result["id"])
		assert.Equal(t, document["orderid"], result["orderid"])
		assert.Equal(t, document["partitionKey"], result["partitionKey"])
		assert.Equal(t, document["nestedproperty"].(map[string]interface{})["subproperty"],
			result["nestedproperty"].(map[string]interface{})["subproperty"])

		// cleanup
		_, err = dbclient.DeleteDocument(result["_self"].(string), documentdb.PartitionKey(result["partitionKey"].(string)))
		assert.NoError(t, err)

		return nil
	}

	testInvokeCreateWithoutPartitionKey := func(ctx flow.Context) error {
		document := createDocument(true, false)
		invokeErr := invokeCreateWithDocument(ctx, document)

		assert.Error(t, invokeErr)
		assert.Contains(t, invokeErr.Error(), "missing partitionKey field")

		return nil
	}

	testInvokeCreateWithoutID := func(ctx flow.Context) error {
		document := createDocument(false, true)
		invokeErr := invokeCreateWithDocument(ctx, document)

		assert.Error(t, invokeErr)
		assert.Contains(t, invokeErr.Error(), "the required properties - 'id; ' - are missing")

		return nil
	}

	testInvokeCreateWithWrongPartitionKey := func(ctx flow.Context) error {
		document := createDocument(true, false)
		document["wrongPartitionKey"] = "somepkvalue"
		invokeErr := invokeCreateWithDocument(ctx, document)

		assert.Error(t, invokeErr)
		assert.Contains(t, invokeErr.Error(), "PartitionKey extracted from document doesn't match the one specified in the header")

		return nil
	}

	flow.New(t, "cosmosdb binding authentication using service principal").
		Step(sidecar.Run(sidecarName,
			embedded.WithoutApp(),
			embedded.WithComponentsPath("./components/serviceprincipal"),
			embedded.WithDaprGRPCPort(currentGRPCPort),
			embedded.WithDaprHTTPPort(currentHTTPPort),
			runtime.WithSecretStores(
				secretstores_loader.New("local.env", func() secretstores.SecretStore {
					return secretstore_env.NewEnvSecretStore(log)
				}),
			),
			runtime.WithOutputBindings(
				bindings_loader.NewOutput("azure.cosmosdb", func() bindings.OutputBinding {
					return cosmosdbbinding.NewCosmosDB(log)
				}),
			))).
		Run()

	ports, err = dapr_testing.GetFreePorts(2)
	assert.NoError(t, err)

	currentGRPCPort = ports[0]
	currentHTTPPort = ports[1]

	flow.New(t, "cosmosdb binding authentication using master key").
		Step(sidecar.Run(sidecarName,
			embedded.WithoutApp(),
			embedded.WithComponentsPath("./components/masterkey"),
			embedded.WithDaprGRPCPort(currentGRPCPort),
			embedded.WithDaprHTTPPort(currentHTTPPort),
			runtime.WithSecretStores(
				secretstores_loader.New("local.env", func() secretstores.SecretStore {
					return secretstore_env.NewEnvSecretStore(log)
				}),
			),
			runtime.WithOutputBindings(
				bindings_loader.NewOutput("azure.cosmosdb", func() bindings.OutputBinding {
					return cosmosdbbinding.NewCosmosDB(log)
				}),
			))).
		Step("verify data sent to output binding is written to Cosmos DB", testInvokeCreateAndVerify).
		Step("expect error if id is missing from document", testInvokeCreateWithoutID).
		Step("expect error if partition key is missing from document", testInvokeCreateWithoutPartitionKey).
		Run()

	ports, err = dapr_testing.GetFreePorts(2)
	assert.NoError(t, err)

	currentGRPCPort = ports[0]
	currentHTTPPort = ports[1]

	flow.New(t, "cosmosdb binding with wrong partition key specified").
		Step(sidecar.Run(sidecarName,
			embedded.WithoutApp(),
			embedded.WithComponentsPath("./components/wrongPartitionKey"),
			embedded.WithDaprGRPCPort(currentGRPCPort),
			embedded.WithDaprHTTPPort(currentHTTPPort),
			runtime.WithSecretStores(
				secretstores_loader.New("local.env", func() secretstores.SecretStore {
					return secretstore_env.NewEnvSecretStore(log)
				}),
			),
			runtime.WithOutputBindings(
				bindings_loader.NewOutput("azure.cosmosdb", func() bindings.OutputBinding {
					return cosmosdbbinding.NewCosmosDB(log)
				}),
			))).
		Step("verify error when wrong partition key used", testInvokeCreateWithWrongPartitionKey).
		Run()
}
