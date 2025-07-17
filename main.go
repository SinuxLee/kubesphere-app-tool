package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/phuslu/log"
	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type AppRequest struct {
	RepoName     string `json:"repoName"`
	Package      string `json:"package"`
	CategoryName string `json:"categoryName"`
	Workspace    string `json:"workspace"`
	AppType      string `json:"appType"`
}

var (
	versionGVR = schema.GroupVersionResource{
		Group:    "application.kubesphere.io",
		Version:  "v2",
		Resource: "applicationversions",
	}
	appGVR = schema.GroupVersionResource{
		Group:    "application.kubesphere.io",
		Version:  "v2",
		Resource: "applications",
	}
	mark          = "openpitrix-import"
	dynamicClient *dynamic.DynamicClient
	serverURL     string
	token         string
	repoURL       string
	limit         int // limit version for each chart
)

func init() {
	log.DefaultLogger = log.Logger{
		TimeFormat: "15:04:05",
		Caller:     1,
		Writer: &log.ConsoleWriter{
			ColorOutput:    true,
			QuoteString:    true,
			EndWithMessage: true,
		},
	}
}

func main() {
	resty_client := resty.New().SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", fmt.Sprintf("Bearer %s", token)).
		SetTimeout(time.Second * 5)

	var rootCmd = &cobra.Command{
		Use:   "app-tool",
		Short: "A CLI tool to manage applications",
		Run: func(cmd *cobra.Command, args []string) {
			if token == "" {
				log.Info().Msg("Using token from /var/run/secrets/kubesphere.io/serviceaccount/token")
				dst := "/var/run/secrets/kubesphere.io/serviceaccount/token"
				data, err := os.ReadFile(dst)
				if err != nil {
					log.Fatal().Msgf("Failed to read token file: %v", err)
				}
				token = string(data)
			}
			run(resty_client)
		},
	}

	rootCmd.Flags().StringVar(&serverURL, "server", "", "Kubesphere Server URL (required)")
	rootCmd.Flags().StringVar(&repoURL, "repo", "", "Helm index URL (required)")
	rootCmd.Flags().StringVar(&token, "token", "", "token (required)")
	rootCmd.Flags().IntVar(&limit, "limit", 1, "limit (option)")

	rootCmd.MarkFlagRequired("server")
	rootCmd.MarkFlagRequired("repo")

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run(resty_client *resty.Client) {
	log.Info().Msgf("Starting to upload to %s ", serverURL)

	err := initDynamicClient()
	if err != nil {
		log.Fatal().Msgf("Failed to initialize dynamic client: %v", err)
	}

	err = uploadChart(resty_client)
	if err != nil {
		log.Fatal().Msgf("Failed to upload chart: %v", err)
	}

	listOptions := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("application.kubesphere.io/app-category-name=%s", mark),
	}

	err = updateAppStatus(listOptions)
	if err != nil {
		log.Fatal().Msgf("[1/4] Failed to update app status: %v", err)
	}
	log.Info().Msgf("[1/4] updateAppStatus completed successfully")

	store := map[string]string{"application.kubesphere.io/app-store": "true"}
	err = updateAppLabel(listOptions, store)
	if err != nil {
		log.Fatal().Msgf("[2/4] Failed to update app label: %v", err)
	}
	log.Info().Msgf("[2/4] updateAppLabel store completed successfully")

	err = updateVersionStatus(listOptions)
	if err != nil {
		log.Fatal().Msgf("[3/4] Failed to update version status: %v", err)
	}
	log.Info().Msgf("[3/4] updateVersionStatus completed successfully")

	categoryName := map[string]string{"application.kubesphere.io/app-category-name": "kubesphere-app-uncategorized"}
	err = updateAppLabel(listOptions, categoryName)
	if err != nil {
		log.Fatal().Msgf("[4/4] Failed to update app category label: %v", err)
	}
	log.Info().Msgf("[4/4] updateAppLabel categoryName completed successfully")
}

func initDynamicClient() (err error) {
	conf := config.GetConfigOrDie()
	dynamicClient, err = dynamic.NewForConfig(conf)
	if err != nil {
		log.Error().Msgf("Failed to create dynamic client: %v", err)
		return err
	}
	log.Info().Msgf("Dynamic client initialized successfully")
	return nil
}

func uploadChart(resty_client *resty.Client) error {
	entry := &repo.Entry{
		URL: repoURL,
	}

	chartRepo, err := repo.NewChartRepository(entry, getter.All(&cli.EnvSettings{}))
	if err != nil {
		log.Fatal().Msgf("failed to create chart repo: %v", err)
	}

	indexPath, err := chartRepo.DownloadIndexFile()
	if err != nil {
		log.Fatal().Msgf("failed to download index file: %v", err)
	}

	indexData, err := repo.LoadIndexFile(indexPath)
	if err != nil {
		log.Fatal().Msgf("failed to load index file: %v", err)
	}

	for _, entries := range indexData.Entries {
		appID := ""
		success := 0

		for _, entry := range entries {
			if entry.Deprecated {
				log.Warn().Msgf("App %s is deprecated, skip", entry.Name)
				break
			}

			// download data
			req := resty_client.R()
			resp, err := req.Get(entry.URLs[0])
			if err != nil {
				log.Error().Msgf("Failed to fetch chart %v, %v", entry.Name, err)
				continue
			}

			if resp.IsError() {
				log.Error().Msgf("Failed to fetch chart %v, status code: %d", entry.Name, resp.StatusCode())
				continue
			}

			// upload data
			var url string
			if appID == "" {
				url = fmt.Sprintf("%s/kapis/application.kubesphere.io/v2/apps", serverURL)
			} else {
				url = fmt.Sprintf("%s/kapis/application.kubesphere.io/v2/apps/%s/versions", serverURL, appID)
			}

			var response struct {
				AppName string `json:"appName"`
			}
			req = resty_client.R().SetBody(AppRequest{
				RepoName:     "upload",
				Package:      base64.StdEncoding.EncodeToString(resp.Body()),
				CategoryName: mark,
				Workspace:    "",
				AppType:      "helm",
			}).SetResult(&response)

			resp, err = req.Post(url)
			if err != nil {
				log.Error().Msgf("Failed to post app version %s:%s %v", entry.Name, entry.Version, err)
				continue
			}

			if resp.IsError() {
				log.Error().Msgf("failed to post app, status code: %d", resp.StatusCode())
				continue
			}

			log.Info().Msgf("App %s:%s posted successfully", entry.Name, entry.Version)
			success++
			if success >= limit {
				break
			}

			time.Sleep(200 * time.Millisecond)
		}
	}

	return nil
}

func updateVersionStatus(listOptions metav1.ListOptions) error {
	list, err := dynamicClient.Resource(appGVR).List(context.TODO(), listOptions)
	if err != nil {
		log.Error().Msgf("Failed to list apps: %v", err)
		return err
	}

	for _, item := range list.Items {
		options := metav1.ListOptions{
			LabelSelector: fmt.Sprintf("application.kubesphere.io/app-id=%s", item.GetName()),
		}
		versionList, err := dynamicClient.Resource(versionGVR).List(context.TODO(), options)
		if err != nil {
			log.Error().Msgf("Failed to list versions for app %s: %v", item.GetName(), err)
			return err
		}

		for _, versionItem := range versionList.Items {
			currentTime := time.Now().UTC().Format(time.RFC3339)
			unstructured.SetNestedField(versionItem.Object, currentTime, "status", "updated")
			unstructured.SetNestedField(versionItem.Object, "admin", "status", "userName")
			unstructured.SetNestedField(versionItem.Object, "active", "status", "state")

			_, err := dynamicClient.Resource(versionGVR).UpdateStatus(context.TODO(), &versionItem, metav1.UpdateOptions{})
			if err != nil {
				log.Error().Msgf("Failed to update version status for app %s: %v", item.GetName(), err)
				return err
			}
		}
	}

	return nil
}

func updateAppLabel(listOptions metav1.ListOptions, label map[string]string) error {
	list, err := dynamicClient.Resource(appGVR).List(context.TODO(), listOptions)
	if err != nil {
		log.Error().Msgf("Failed to list apps: %v", err)
		return err
	}

	for _, item := range list.Items {
		labels := item.GetLabels()
		for k, v := range label {
			labels[k] = v
		}

		item.SetLabels(labels)
		_, err = dynamicClient.Resource(appGVR).Update(context.TODO(), &item, metav1.UpdateOptions{})
		if err != nil {
			log.Error().Msgf("Failed to update labels for app %s: %v", item.GetName(), err)
			return err
		}
	}

	return nil
}

func updateAppStatus(listOptions metav1.ListOptions) error {
	list, err := dynamicClient.Resource(appGVR).List(context.TODO(), listOptions)
	if err != nil {
		log.Error().Msgf("Failed to list apps: %v", err)
		return err
	}

	for _, item := range list.Items {
		currentTime := time.Now().UTC().Format(time.RFC3339)
		unstructured.SetNestedField(item.Object, "active", "status", "state")
		unstructured.SetNestedField(item.Object, currentTime, "status", "updateTime")

		_, err := dynamicClient.Resource(appGVR).UpdateStatus(context.TODO(), &item, metav1.UpdateOptions{})
		if err != nil {
			log.Error().Msgf("Failed to update status for app %s: %v", item.GetName(), err)
			return err
		}
	}

	return nil
}
