// Command driver plays the operator and GitHub for the deploy E2E's platform
// part, through the platform API only (pkg/client):
//
//	driver register -repo e2e/demo -branch main -env kind      register; prints the first deployment's id
//	driver push -repo e2e/demo -branch main -after <sha>       a signed push webhook; prints the deployment's id
//	                [-instance demo]                           and fails unless it deploys that instance
//	driver watch -id <deployment> -want healthy                follow the event stream until it ends
//	driver delete -app demo                                    unregister
//	driver add-instance -app moimroom -branch dev              add a branch instance; prints the first deployment's id and the instance
//	driver delete-instance -app moimroom -instance moimroom--dev
//	driver last -app moimroom -branch main                     prints the id and status of the branch's newest deployment
//	driver deployment -id <deployment>                         prints its trigger, build id ("-" without one) and first image
//	driver secret-set -app store -name A -value v [-instance]  set an app secret (PUT /v1/apps/{name}/secrets)
//	driver secret-get -app store -name A [-instance]           print an app secret's value
//	driver redeploy -app store [-branch main]                  redeploy an instance; prints the deployment's id
//	driver s3 -endpoint host:port -access K -secret S -bucket B -want ok|denied|badkey|missing
//	                                                           check what a key may do with a bucket
//	                                                           (through HTTP_PROXY, as the cluster names it)
//
// The operator token is E2E_OPERATOR_TOKEN, the webhook secret
// E2E_WEBHOOK_SECRET. watch prints every phase with the seconds since it
// started and exits 1 unless the deployment ends with -want.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: driver register|push|watch|delete|add-instance|delete-instance|last|deployment|secret-set|secret-get|redeploy|s3 [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	api := fs.String("api", "http://127.0.0.1:18380", "platform API")
	repo := fs.String("repo", "", "owner/name")
	branch := fs.String("branch", "main", "branch")
	env := fs.String("env", "kind", "environment")
	after := fs.String("after", "", "the pushed commit")
	id := fs.String("id", "", "deployment id")
	want := fs.String("want", "healthy", "the final status watch expects")
	app := fs.String("app", "", "app name")
	instance := fs.String("instance", "", "instance name")
	name := fs.String("name", "", "secret name")
	value := fs.String("value", "", "secret value")
	endpoint := fs.String("endpoint", "", "S3 endpoint host:port")
	access := fs.String("access", "", "S3 access key")
	secret := fs.String("secret", "", "S3 secret key")
	bucket := fs.String("bucket", "", "S3 bucket")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after this long")
	_ = fs.Parse(os.Args[2:])

	c := client.New(*api, os.Getenv("E2E_OPERATOR_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch os.Args[1] {
	case "register":
		auto := true
		created, err := c.CreateApp(ctx, apiv1.AppCreateRequest{Repo: *repo, Branch: branch, Environments: []string{*env}, AutoDeploy: &auto})
		if err != nil {
			log.Fatal("register: ", err)
		}
		fmt.Println(created.Deployment.Id)
	case "push":
		payload, _ := json.Marshal(map[string]any{
			"ref": "refs/heads/" + *branch, "after": *after, "deleted": false,
			"repository": map[string]any{"full_name": *repo},
			"pusher":     map[string]any{"name": "e2e"}, "sender": map[string]any{"login": "e2e"},
		})
		list, err := c.SendGitHubWebhook(ctx, "push", fmt.Sprintf("e2e-%d", time.Now().UnixNano()), payload, os.Getenv("E2E_WEBHOOK_SECRET"))
		if err != nil {
			log.Fatal("push: ", err)
		}
		if len(list.Deployments) != 1 {
			log.Fatalf("push queued %d deployments, want 1", len(list.Deployments))
		}
		if d := list.Deployments[0]; *instance != "" && d.Instance != *instance {
			log.Fatalf("push queued deployment %s of instance %s, want %s", d.Id, d.Instance, *instance)
		}
		fmt.Println(list.Deployments[0].Id)
	case "watch":
		began := time.Now()
		var last *apiv1.Deployment
		err := c.WatchDeployment(ctx, *id, func(m client.DeploymentMessage) error {
			at := time.Since(began).Seconds()
			switch {
			case m.Snapshot != nil:
				last = m.Snapshot
				fmt.Printf("%6.1fs snapshot %s\n", at, m.Snapshot.Status)
			case m.Event.Type == apiv1.DeploymentEventTypeLog:
				// build output: too much for the test log
			default:
				fmt.Printf("%6.1fs %s %s %s %s\n", at, m.Event.Type, deref(m.Event.Phase), deref(m.Event.Env), oneLine(deref(m.Event.Message)))
			}
			return nil
		})
		if err != nil {
			log.Fatal("watch: ", err)
		}
		fmt.Printf("%s %s after %.1fs", *id, last.Status, time.Since(began).Seconds())
		if last.Error != nil {
			fmt.Printf(": %s", oneLine(*last.Error))
		}
		fmt.Println()
		if string(last.Status) != *want {
			os.Exit(1)
		}
	case "delete":
		if err := c.DeleteApp(ctx, *app); err != nil {
			log.Fatal("delete: ", err)
		}
	case "add-instance":
		auto := true
		created, err := c.CreateAppInstance(ctx, *app, apiv1.AppInstanceCreateRequest{Branch: *branch, AutoDeploy: &auto})
		if err != nil {
			log.Fatal("add instance: ", err)
		}
		fmt.Println(created.Deployment.Id, created.Instance.Name)
	case "delete-instance":
		if err := c.DeleteAppInstance(ctx, *app, *instance); err != nil {
			log.Fatal("delete instance: ", err)
		}
	case "last":
		list, err := c.ListBranchDeployments(ctx, *app, *branch, 1)
		if err != nil {
			log.Fatal("list deployments: ", err)
		}
		if len(list.Deployments) == 0 {
			log.Fatalf("%s has no deployment of branch %s", *app, *branch)
		}
		fmt.Println(list.Deployments[0].Id, list.Deployments[0].Status)
	case "deployment":
		d, err := c.GetDeployment(ctx, *id)
		if err != nil {
			log.Fatal("deployment: ", err)
		}
		image := "-"
		if len(d.Images) > 0 {
			image = d.Images[0].Image
		}
		build := deref(d.BuildId)
		if build == "" {
			build = "-"
		}
		fmt.Println(d.Trigger, build, image)
	case "secret-set":
		req := apiv1.AppSecretsUpdate{Set: &map[string]string{*name: *value}}
		if *instance != "" {
			req.Instance = instance
		}
		if _, err := c.UpdateAppSecrets(ctx, *app, req); err != nil {
			log.Fatal("set secret: ", err)
		}
	case "secret-get":
		v, err := c.GetAppSecret(ctx, *app, *name, *instance)
		if err != nil {
			log.Fatal("get secret: ", err)
		}
		fmt.Println(v.Value)
	case "redeploy":
		yes := true
		d, err := c.CreateDeployment(ctx, *app, apiv1.DeploymentCreateRequest{Branch: branch, Redeploy: &yes})
		if err != nil {
			log.Fatal("redeploy: ", err)
		}
		fmt.Println(d.Id)
	case "s3":
		if err := checkS3(ctx, *endpoint, *access, *secret, *bucket, *want); err != nil {
			log.Fatal("s3: ", err)
		}
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

// checkS3 checks what the key may do with the bucket: want ok writes, reads
// back and deletes an object; denied expects AccessDenied on a write;
// badkey expects the key to be unknown; missing expects no such bucket.
func checkS3(ctx context.Context, endpoint, access, secret, bucket, want string) error {
	cl, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Region: "us-east-1"})
	if err != nil {
		return err
	}
	put := func() error {
		_, err := cl.PutObject(ctx, bucket, "e2e-check.txt", strings.NewReader("ok"), 2, minio.PutObjectOptions{})
		return err
	}
	switch want {
	case "ok":
		if err := put(); err != nil {
			return fmt.Errorf("write %s: %w", bucket, err)
		}
		o, err := cl.GetObject(ctx, bucket, "e2e-check.txt", minio.GetObjectOptions{})
		if err != nil {
			return fmt.Errorf("read %s: %w", bucket, err)
		}
		b, err := io.ReadAll(o)
		o.Close()
		if err != nil || string(b) != "ok" {
			return fmt.Errorf("read %s: %q, %v", bucket, b, err)
		}
		return cl.RemoveObject(ctx, bucket, "e2e-check.txt", minio.RemoveObjectOptions{})
	case "denied", "badkey":
		code := minio.ToErrorResponse(put()).Code
		wantCode := map[string]string{"denied": "AccessDenied", "badkey": "InvalidAccessKeyId"}[want]
		if code != wantCode {
			return fmt.Errorf("a write to %s answered %q, want %s", bucket, code, wantCode)
		}
		return nil
	case "missing":
		ok, err := cl.BucketExists(ctx, bucket)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("bucket %s exists", bucket)
		}
		return nil
	}
	return fmt.Errorf("unknown -want %q", want)
}

func deref[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func oneLine(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strings.ReplaceAll(s, "\n", " ")
}
