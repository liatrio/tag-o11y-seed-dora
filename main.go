package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

var (
	logger                     *zap.Logger
	doraElitePerformanceLevel  *DoraPerformanceLevel
	doraHighPerformanceLevel   *DoraPerformanceLevel
	doraMediumPerformanceLevel *DoraPerformanceLevel
	doraLowPerformanceLevel    *DoraPerformanceLevel
	daysBackToGenerateData     int
	ghaEventPayload            *GHAEventPayload
	otelWebhookUrl             string
)

type DoraPerformanceLevel struct {
	Level                  string
	DaysBetweenDeployments int
}

type GHAEventPayload struct {
	DeploymentCreatedPayloadPath string
	IssueCreatedPayloadPath      string
	PullRequestClosedPayloadPath string
}

// Elite:
// 	Deployment Frequency: On-demand (multiple deploys per day)
// 	Lead Time for Changes: Less than one day
// 	Time to Restore Service: Less than one hour
// 	Change Failure Rate: 0-15%

// High:
// 	Deployment Frequency: Between once per day and once per week
// 	Lead Time for Changes: Between one day and one week
// 	Time to Restore Service: Less than one day
// 	Change Failure Rate: 0-15%

// Medium:
// 	Deployment Frequency: Between once per week and once per month
// 	Lead Time for Changes: Between one week and one month
// 	Time to Restore Service: Less than one day
// 	Change Failure Rate: 0-30%
// Low:
// 	Deployment Frequency: Between once per month and once every six months
// 	Lead Time for Changes: Between one month and six months
// 	Time to Restore Service: Between one day and one week
// 	Change Failure Rate: 0-45%

func init() {
	var err error

	ghaEventPayload = &GHAEventPayload{
		DeploymentCreatedPayloadPath: "./data/deployment_event-flattened.json",
		IssueCreatedPayloadPath:      "./data/incident_created_event-flattened.json",
		PullRequestClosedPayloadPath: "./data/pull_request_closed_event-flattened.json",
	}

	doraElitePerformanceLevel = &DoraPerformanceLevel{
		Level:                  "elite",
		DaysBetweenDeployments: 0, // Will be treated as a special case to indicate multiple deploys per day
	}

	doraHighPerformanceLevel = &DoraPerformanceLevel{
		Level:                  "high",
		DaysBetweenDeployments: 1,
	}

	doraMediumPerformanceLevel = &DoraPerformanceLevel{
		Level:                  "medium",
		DaysBetweenDeployments: 8,
	}

	doraLowPerformanceLevel = &DoraPerformanceLevel{
		Level:                  "low",
		DaysBetweenDeployments: 31,
	}

	// daysBackToGenerateData = 183 // 6 months
	daysBackToGenerateData = 31 // 6 months

	rawJSON := []byte(`{
        "level": "debug",
        "encoding": "json",
        "outputPaths": ["stdout"],
        "errorOutputPaths": ["stderr"],
        "initialFields": {"service": "seed-dora-logs"},
        "encoderConfig": {
            "messageKey": "message",
            "levelKey": "level",
            "levelEncoder": "lowercase"
            }
        }
    `)

	var cfg zap.Config
	if err = json.Unmarshal(rawJSON, &cfg); err != nil {
		panic(err)
	}

	logger = zap.Must(cfg.Build())
	defer func() {
		if err := logger.Sync(); err != nil {
			return
		}
	}()
}

// InPlaceUpdateJSONValue updates the value of a JSON field in a map in place.
// The path is a dot-separated string that represents the path to the field.
// The newValue is the new value to set.
// The function returns an error if the path is not found.
func InPlaceUpdateJSONValue(data map[string]interface{}, path string, newValue interface{}) error {
	keys := strings.Split(path, ".")
	lastKey := keys[len(keys)-1]

	// Traverse the map according to the path
	var current interface{} = data
	for _, key := range keys[:len(keys)-1] {
		switch currentMap := current.(type) {
		case map[string]interface{}:
			current = currentMap[key]
		default:
			return fmt.Errorf("path not found: %s", path)
		}
	}

	// Update the value at the last key
	switch currentMap := current.(type) {
	case map[string]interface{}:
		currentMap[lastKey] = newValue
	default:
		return fmt.Errorf("path not found: %s", path)
	}

	return nil
}

func stringReplaceFirst(b []byte, pattern string, repl string) []byte {
	re := regexp.MustCompile(fmt.Sprintf(`(.*?)%s(.*)`, regexp.QuoteMeta(pattern)))
	updatedContent := re.ReplaceAll(b, []byte("${1}"+repl+"${2}"))
	return updatedContent
}

func generateTimestamps(count int, interval time.Duration) []string {
	timestamps := make([]string, count)
	for i := 1; i <= count; i++ {
		t := time.Now().Add(-interval * time.Duration(i))
		timestamps[count-i] = t.Format("2006-01-02T15:04:05Z")
	}

	return timestamps
}

func sendPayload(client *http.Client, url string, payload []byte) {
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		logger.Sugar().Error("Failed to send request: ", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Sugar().Error("Failed to read response body: ", err)
		return
	}

	if len(body) != 0 {
		logger.Sugar().Info("Response body: ", string(body))
	}

	if resp.StatusCode != http.StatusOK {
		logger.Sugar().Errorf("Failed to send request. Status code: %d, Response: %s", resp.StatusCode, string(body))
		return
	}
	time.Sleep(500 * time.Millisecond)
}

// 	client := &http.Client{
// 		Timeout: time.Second * 10,
// 	}
//
// 	file, err := os.Open(filePath)
// 	if err != nil {
// 		logger.Sugar().Errorf("Failed to open %s: %v", filePath, err)
// 		return
// 	}
// 	defer file.Close()
//
// 	payload, err := io.ReadAll(file)
// 	if err != nil {
// 		logger.Sugar().Error("Failed to read deploy.json: ", err)
// 		return
// 	}
//
// 	// Months of data
// 	numberOfMonths := 6
// 	var numberOfTimestamps int
// 	timestamps := generateTimestamps(20)
// 	logger.Sugar().Infof("Timestamps: %v\n", timestamps)
//
// 	// var wg sync.WaitGroup
// 	for _, ts := range timestamps {
// 		wg.Add(1)
//
// 		go func() {
// 			defer wg.Done()
// 			switch filePath {
// 			case "./data/deployment_event-flattened.json":
// 				logger.Sugar().Infof("Sending deployment event for %s Dora Performance Level", doraTeam.Level)
// 				payload, err := createDeploymentPayload(doraTeam)
// 			case "./data/incident_created_event-flattened.json":
// 				logger.Sugar().Error("Not implemented yet")
// 			case "./data/pull_request_closed_event-flattened.json":
// 				logger.Sugar().Error("Not implemented yet")
// 			default:
// 			}
//
// 			// go sendPayload(ctx, &wg, client, url, data, ts)
// 			sendPayload(ctx, client, otelWebhookUrl, payload, ts)
// 		}()
// 	}
//
// 	// var data map[string]interface{}
// 	// if err := json.Unmarshal(payload, &data); err != nil {
// 	// 	logger.Sugar().Error("Failed to unmarshal deploy.json: ", err)
// 	// 	return
// 	// }
//
// 	// Handle Elite Dora Performance Level
//
// 	wg.Wait()
//
// 	logger.Sugar().Infof("Successfully sent %s to the URL", filePath)
// 	logger.Sugar().Info("Successfully pushed logs to Loki")
// }

func main() {
	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()

	otelWebhookUrl = os.Getenv("OTEL_WEBHOOK_URL")
	if otelWebhookUrl == "" {
		logger.Sugar().Error("OTEL_WEBHOOK_URL is not set")
		return
	}

	doraTeams := []DoraPerformanceLevel{
		*doraElitePerformanceLevel,
		*doraHighPerformanceLevel,
		*doraMediumPerformanceLevel,
		*doraLowPerformanceLevel,
	}

	//payloadFilePaths := []string{"./data/deployment_event-flattened.json"}
	// dataPaths := []string{"./data/deployment_event-flattened.json", "./data/incident_created_event-flattened.json", "./data/pull_request_closed_event-flattened.json"}

	doraWg := sync.WaitGroup{}
	for _, doraTeam := range doraTeams {
		doraWg.Add(1)
		go func(doraTeam DoraPerformanceLevel) {
			defer doraWg.Done()
			logger.Sugar().Infof("Dora Performance Level: %s\n", doraTeam.Level)
			genDeploymentFrequencyEvents(doraTeam)
		}(doraTeam)
	}

	doraWg.Wait()
	// for _, path := range payloadFilePaths {
	// 	//sendFilePayloadsWithContext(ctx, url, path)
	// }
	logger.Sugar().Info("Successfully sent all payloads")
}

func genDeploymentFrequencyEvents(doraTeam DoraPerformanceLevel) {
	client := &http.Client{
		Timeout: time.Second * 10,
	}

	file, err := os.Open(ghaEventPayload.DeploymentCreatedPayloadPath)
	if err != nil {
		logger.Sugar().Errorf("Failed to open %s: %v", ghaEventPayload.DeploymentCreatedPayloadPath, err)
		return
	}
	defer file.Close()

	payload, err := io.ReadAll(file)
	if err != nil {
		logger.Sugar().Error("Failed to read deploy.json: ", err)
		return
	}

	// Calculate the number of events we need to send based on the total number
	// of months we want data for and the number of days between deployments
	var deploymentEvents int
	var interval time.Duration
	if doraTeam.Level == "elite" {
		deploymentEvents = daysBackToGenerateData * 2
		interval = time.Hour * 12
	} else {
		deploymentEvents = daysBackToGenerateData / doraTeam.DaysBetweenDeployments
		interval = time.Hour * 24 * time.Duration(doraTeam.DaysBetweenDeployments)
	}
	logger.Sugar().Infof("Generating %v deployment events at %v intervals for %s Dora Performance Level", deploymentEvents, interval, doraTeam.Level)

	createdAtTimestamps := generateTimestamps(deploymentEvents, interval)
	logger.Sugar().Infof("Dora Performance Level: %s, Created %v Timestamps\n", doraTeam.Level, len(createdAtTimestamps))

	for _, ts := range createdAtTimestamps {
		newPayload := stringReplaceFirst(payload, "CREATED_AT_UPDATE_ME", ts)
		sendPayload(client, otelWebhookUrl, newPayload)
	}
}
