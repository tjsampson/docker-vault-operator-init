package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	//"google.golang.org/protobuf/types/known/fieldmaskpb"

	//"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/genproto/protobuf/field_mask"

	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	GcpSecretName               string `envconfig:"GCP_SECRET_NAME" required:"true"`
	ProjectID                   string `envconfig:"GCP_PROJECT_ID" required:"true"`
	GoogleApplicationCredential string `envconfig:"GOOGLE_APPLICATION_CREDENTIAL" required:"true"`
	VaultAddr                   string `envconfig:"VAULT_ADDR" required:"true"`
	VaultRecoveryShares         int    `envconfig:"VAULT_RECOVERY_SHARES" default:"5"`
	VaultRecoveryThreshold      int    `envconfig:"VAULT_RECOVERY_THRESHOLD" default:"3"`
}

func GetConfig() Config {
	c := Config{}
	err := envconfig.Process("", &c)
	if err != nil {
		panic(err)
	}

	return c
}

type Client struct {
	client    *secretmanager.Client
	projectID string
}

func NewClient(cfg *Config) (*Client, error) {
	// Create the client.
	ctx := context.Background()
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Panic("failed to setup client: %v", err.Error())
		return nil, err
	}

	return &Client{
		client:    client,
		projectID: cfg.ProjectID,
	}, nil
}

func (c *Client) secretPath(name string) string {
	return fmt.Sprintf("projects/%s/secrets/%s/versions/latest", c.projectID, name)
}

func (c *Client) getSecret(name, key string) (string, error) {
	// Call the API.
	secret, err := c.client.AccessSecretVersion(context.TODO(), &secretmanagerpb.AccessSecretVersionRequest{Name: c.secretPath(name)})
	if err != nil {
		return "", err
	}

	jsonMap := make(map[string]interface{})
	err = json.Unmarshal(secret.Payload.Data, &jsonMap)
	if err != nil {
		panic(err)
	}

	if val, ok := jsonMap[key]; ok {
		if rs, ok := val.(string); ok {
			return rs, nil
		}
	}

	return "", fmt.Errorf("unknown secret name:%s and key:%s", name, key)
}

// ContextWithSignal returns a context that cancels itself when given signals are received.
func contextWithSignal(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
			cancel()
		}
		signal.Stop(ch)
		close(ch)
	}()
	return ctx, cancel
}

func main() {
	var ctx, cancel = contextWithSignal(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	c := GetConfig()

	cli, err := NewClient(&c)
	if err != nil {
		log.Panic(err)
	}
	defer cli.client.Close()

	secret, err := cli.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: cli.secretPath(c.GcpSecretName)})
	if err != nil {
		log.Println("failed to access secret version")
		if strings.Contains(strings.ToLower(err.Error()), "notfound") {
			log.Println("secret not found")
			// Create the request to create the secret.
			createSecretReq := &secretmanagerpb.CreateSecretRequest{
				Parent:   fmt.Sprintf("projects/%s", c.ProjectID),
				SecretId: c.GcpSecretName,
				Secret: &secretmanagerpb.Secret{
					Replication: &secretmanagerpb.Replication{
						Replication: &secretmanagerpb.Replication_Automatic_{
							Automatic: &secretmanagerpb.Replication_Automatic{},
						},
					},
				},
			}

			secret, err := cli.client.CreateSecret(ctx, createSecretReq)
			if err != nil {
				log.Fatalf("failed to create secret: %v", err)
			}
			// Declare the payload to store.
			payload := []byte("comming soon")
			log.Println("secret created")

			// Build the request.
			addSecretVersionReq := &secretmanagerpb.AddSecretVersionRequest{
				Parent: secret.Name,
				Payload: &secretmanagerpb.SecretPayload{
					Data: payload,
				},
			}

			// Call the API.
			_, err = cli.client.AddSecretVersion(ctx, addSecretVersionReq)
			if err != nil {
				log.Fatalf("failed to add secret version: %v", err)
			}

			log.Println("secret versioned")
		} else {
			log.Panic(err.Error())
		}
	}

	if secret == nil {
		log.Println("nil secret first time creation")
	} else {
		log.Println(secret.Name)
	}

	updateReq := &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			//Name: c.GcpSecretName,
			Name: fmt.Sprintf("projects/%s/secrets/%s", c.ProjectID, c.GcpSecretName),
			Labels: map[string]string{
				"secretmanager": "rocks",
			},
		},
		UpdateMask: &field_mask.FieldMask{
			Paths: []string{"labels"},
		},
	}

	// Call the API.
	_, err = cli.client.UpdateSecret(ctx, updateReq)
	if err != nil {
		log.Fatalf("failed to update secret: %v", err)
	}

	// Build the request.
	addSecretVersionReq := &secretmanagerpb.AddSecretVersionRequest{
		Parent: fmt.Sprintf("projects/%s/secrets/%s", c.ProjectID, c.GcpSecretName),
		Payload: &secretmanagerpb.SecretPayload{
			Data: []byte("updated"),
		},
	}

	_, err = cli.client.AddSecretVersion(ctx, addSecretVersionReq)
	if err != nil {
		log.Fatalf("failed to add secret version: %v", err)
	}

	client := &http.Client{}
	var jsonStr = []byte(fmt.Sprintf(`{"recovery_shares":%d, "recovery_threshold": %d}`, c.VaultRecoveryShares, c.VaultRecoveryThreshold))
	retries := 0
	resp := &http.Response{}
	for retries < 10 {
		req, err := http.NewRequest("PUT", fmt.Sprintf("%s/v1/sys/init", c.VaultAddr), bytes.NewBuffer(jsonStr))
		if err != nil {
			log.Panic(err)
		}
		time.Sleep(10 * time.Second)

		resp, err = client.Do(req)
		if err != nil {
			log.Println(err)
			retries += 1
			continue
		}

		if resp != nil && resp.StatusCode != 200 {
			bodyBytes, _ := ioutil.ReadAll(resp.Body)
			if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(bodyBytes), "already initialized") {
				log.Println("already unsealed")
				os.Exit(0)
				return
			}
			log.Printf("status: %d, body: %s, retrying...", resp.StatusCode, string(bodyBytes))
			retries += 1
			continue
		}
		break
	}

	if resp != nil && resp.StatusCode != 200 {
		log.Panicf("invalid status: %d", resp.StatusCode)
	}

	var respMap map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&respMap)
	if err != nil {
		log.Panic(err)
	}

	if _, ok := respMap["root_token"]; !ok {
		log.Panic("missing root token")
	}

	secretString, err := json.Marshal(respMap)
	if err != nil {
		log.Panic(err)
	}

	updateReqNew := &secretmanagerpb.UpdateSecretRequest{
		Secret: &secretmanagerpb.Secret{
			Name: fmt.Sprintf("projects/%s/secrets/%s", c.ProjectID, c.GcpSecretName),
			Labels: map[string]string{
				"secretmanager": "rocks",
			},
		},
		UpdateMask: &field_mask.FieldMask{
			Paths: []string{"labels"},
		},
	}

	// Call the API.
	_, err = cli.client.UpdateSecret(ctx, updateReqNew)
	if err != nil {
		log.Fatalf("failed to update secret: %v", err)
	}

	// Build the request.
	addSecretVersionReqNew := &secretmanagerpb.AddSecretVersionRequest{
		Parent:  fmt.Sprintf("projects/%s/secrets/%s", c.ProjectID, c.GcpSecretName),
		Payload: &secretmanagerpb.SecretPayload{Data: secretString},
	}
	// Call the API.
	_, err = cli.client.AddSecretVersion(ctx, addSecretVersionReqNew)
	if err != nil {
		log.Fatalf("failed to add secret version: %v", err)
	}
}
