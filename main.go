package main

import (
	"bytes"
	"context"
	"crypto/sha1"
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
	logger                 *zap.Logger
	doraEliteTeam          *DoraTeam
	doraHighTeam           *DoraTeam
	doraMediumTeam         *DoraTeam
	doraLowTeam            *DoraTeam
	daysBackToGenerateData int
	ghaEventPayload        *GHAEventPayload
	otelWebhookUrl         string
)

type DoraTeam struct {
	Level                   string
	DaysBetweenDeployments  int
	MinutesToRestoreService int
	RepoName                string
	ChangeFailurePercent    float64
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
//
// High:
// 	Deployment Frequency: Between once per day and once per week
// 	Lead Time for Changes: Between one day and one week
// 	Time to Restore Service: Less than one day
// 	Change Failure Rate: 0-15%
//
// Medium:
// 	Deployment Frequency: Between once per week and once per month
// 	Lead Time for Changes: Between one week and one month
// 	Time to Restore Service: Less than one day
// 	Change Failure Rate: 0-30%
//
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

	doraEliteTeam = &DoraTeam{
		Level:                   "elite",
		DaysBetweenDeployments:  0, // Will be treated as a special case to indicate multiple deploys per day
		MinutesToRestoreService: 30,
		RepoName:                "dora-elite-repo",
		ChangeFailurePercent:    .05,
	}

	doraHighTeam = &DoraTeam{
		Level:                   "high",
		DaysBetweenDeployments:  1,
		MinutesToRestoreService: 180, // 3 hours
		RepoName:                "dora-high-repo",
		ChangeFailurePercent:    .10,
	}

	doraMediumTeam = &DoraTeam{
		Level:                   "medium",
		DaysBetweenDeployments:  8,
		MinutesToRestoreService: 480, // 8 hours
		RepoName:                "dora-medium-repo",
		ChangeFailurePercent:    .25,
	}

	doraLowTeam = &DoraTeam{
		Level:                   "low",
		DaysBetweenDeployments:  31,
		MinutesToRestoreService: 4320, // 3 days
		RepoName:                "dora-low-repo",
		ChangeFailurePercent:    .40,
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

func getEvenlyDistributedIndexes(timestamps []time.Time, M int) []int {
	N := len(timestamps)
	if M >= N {
		// If M is greater than or equal to N, return all indexes
		indexes := make([]int, N)
		for i := range indexes {
			indexes[i] = i
		}
		return indexes
	}

	// Calculate the step size
	step := float64(N-1) / float64(M-1)
	indexes := make([]int, M)

	for i := 0; i < M; i++ {
		indexes[i] = int(float64(i) * step)
	}

	return indexes
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

func generateTimestamps(count int, interval time.Duration) []time.Time {
	timestamps := make([]time.Time, count)
	for i := 1; i <= count; i++ {
		t := time.Now().Add(-interval * time.Duration(i))
		// timestamps[count-i] = t.Format("2006-01-02T15:04:05Z")
		timestamps[count-i] = t
	}

	return timestamps
}

func generateCommitSha(timestamp string) string {
	sha := sha1.New()
	sha.Write([]byte(timestamp))
	bs := sha.Sum(nil)
	return fmt.Sprintf("%x", bs)
}

func sendPayload(client *http.Client, url string, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		logger.Sugar().Error("Failed to create request: ", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)

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
}

func main() {
	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()

	otelWebhookUrl = os.Getenv("OTEL_WEBHOOK_URL")
	if otelWebhookUrl == "" {
		logger.Sugar().Error("OTEL_WEBHOOK_URL is not set")
		return
	}

	doraTeams := []DoraTeam{
		*doraEliteTeam,
		*doraHighTeam,
		*doraMediumTeam,
		*doraLowTeam,
	}

	doraWg := sync.WaitGroup{}
	for _, doraTeam := range doraTeams {
		doraWg.Add(1)
		go func(doraTeam DoraTeam) {
			defer doraWg.Done()
			logger.Sugar().Infof("Dora Performance Level: %s\n", doraTeam.Level)

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

			// Generate successful deploy events for Deployment Frequency
			successfulDeployTimestamps := generateTimestamps(deploymentEvents, interval)
			sendDeploymentEvent(doraTeam, successfulDeployTimestamps)

			// Generate Deployment and Incidents for Change Failure Rate and Time to Restore Service
			deployTimestampsThatCauseIncidents, incidentTimestamps := createIncidentTimestamps(doraTeam, successfulDeployTimestamps)
			sendDeploymentEvent(doraTeam, deployTimestampsThatCauseIncidents)
			sendIncidentEvents(doraTeam, incidentTimestamps)
		}(doraTeam)
	}

	doraWg.Wait()
	logger.Sugar().Info("Successfully sent all payloads")
}

// A deploy is considered successful if it is not followed by an incident
// So in order to not mess with the deployment frequency, we will generate
// a successful deploy event followed by an incident before the passed in
// timestamp of a successful deploy. This should result in us being able to
// tell the incident response time. While not effecting the deployment frequency
//
// Example:
// Successful Deploy at 2024-04-10T12:00:00Z - Existing deploy for daily frequency
// Successful Deploy at 2024-04-11T12:00:00Z - Existing deploy for daily frequency
// Successful Deploy at 2024-04-12T13:00:00Z - New failed deploy
// Incident at 2024-04-11T14:00:00Z          - Incident response time is 1 hour
// Successful Deploy at 2024-04-12T12:00:00Z - Existing deploy for daily frequency
//
// We need to generate enough incidents to fit the Change Failure Rate
// And recover in the correct amount of time for the Time to Restore Service
func createIncidentTimestamps(doraTeam DoraTeam, successfulDeployTimestamps []time.Time) (deployTimestampsThatCauseIncidents []time.Time, incidentTimestamps []time.Time) {
	// Elite:
	// 	Deployment Frequency: On-demand (multiple deploys per day)
	// 	Lead Time for Changes: Less than one day
	// 	Time to Restore Service: Less than one hour
	// 	Change Failure Rate: 0-15%
	//
	// High:
	// 	Deployment Frequency: Between once per day and once per week
	// 	Lead Time for Changes: Between one day and one week
	// 	Time to Restore Service: Less than one day
	// 	Change Failure Rate: 0-15%
	//
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
	numberOfIncidentsForChangeFailureRate := int(float64(len(successfulDeployTimestamps)) * doraTeam.ChangeFailurePercent)
	if numberOfIncidentsForChangeFailureRate == 0 {
		numberOfIncidentsForChangeFailureRate = 1 // always generate at least 1 incident
	}

	indexes := getEvenlyDistributedIndexes(successfulDeployTimestamps, numberOfIncidentsForChangeFailureRate)

	for _, index := range indexes {
		incidentTimestamps = append(
			incidentTimestamps,
			successfulDeployTimestamps[index].Add(-time.Duration(doraTeam.MinutesToRestoreService)*time.Minute),
		)
	}
	for _, incidentTimestamp := range incidentTimestamps {
		deployTimestampsThatCauseIncidents = append(deployTimestampsThatCauseIncidents, incidentTimestamp.Add(-10*time.Minute))
	}

	return deployTimestampsThatCauseIncidents, incidentTimestamps
}

// Returns a slice of timestamps corresponding to when successful deployments were created
func sendIncidentEvents(doraTeam DoraTeam, issueTimestamps []time.Time) {
	logger.Sugar().Infof("Sending %v incident events for %s Dora Performance Level\n", len(issueTimestamps), doraTeam.Level)
	client := &http.Client{
		Timeout: time.Second * 10,
	}

	file, err := os.Open(ghaEventPayload.IssueCreatedPayloadPath)
	if err != nil {
		logger.Sugar().Errorf("Failed to open %s: %v", ghaEventPayload.IssueCreatedPayloadPath, err)
		return
	}
	defer file.Close()

	payload, err := io.ReadAll(file)
	if err != nil {
		logger.Sugar().Error("Failed to read deploy.json: ", err)
		return
	}

	payload = stringReplaceFirst(payload, "REPO_NAME_UPDATE_ME", doraTeam.RepoName)

	// Calculate the number of events we need to send based on the total number
	// of months we want data for and the number of days between deployments
	// var deploymentEvents int
	// var interval time.Duration
	// if doraTeam.Level == "elite" {
	// 	deploymentEvents = daysBackToGenerateData * 2
	// 	interval = time.Hour * 12
	// } else {
	// 	deploymentEvents = daysBackToGenerateData / doraTeam.DaysBetweenDeployments
	// 	interval = time.Hour * 24 * time.Duration(doraTeam.DaysBetweenDeployments)
	// }
	// logger.Sugar().Infof("Generating %v deployment events at %v intervals for %s Dora Performance Level", deploymentEvents, interval, doraTeam.Level)

	// createdAtTimestamps := generateTimestamps(deploymentEvents, interval)
	logger.Sugar().Infof("Dora Performance Level: %s, Created %v Timestamps\n", doraTeam.Level, len(issueTimestamps))

	requestWg := sync.WaitGroup{}
	for _, ts := range issueTimestamps {
		newPayload := stringReplaceFirst(payload, "CREATED_AT_UPDATE_ME", ts.Format("2006-01-02T15:04:05Z"))
		requestWg.Add(1)
		go func(payload []byte) {
			defer requestWg.Done()
			sendPayload(client, otelWebhookUrl, payload)
		}(newPayload)
	}
	requestWg.Wait()
}
func sendDeploymentEvent(doraTeam DoraTeam, successfulDeployTimestamps []time.Time) {
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

	payload = stringReplaceFirst(payload, "REPO_NAME_UPDATE_ME", doraTeam.RepoName)

	// Calculate the number of events we need to send based on the total number
	// of months we want data for and the number of days between deployments
	// var deploymentEvents int
	// var interval time.Duration
	// if doraTeam.Level == "elite" {
	// 	deploymentEvents = daysBackToGenerateData * 2
	// 	interval = time.Hour * 12
	// } else {
	// 	deploymentEvents = daysBackToGenerateData / doraTeam.DaysBetweenDeployments
	// 	interval = time.Hour * 24 * time.Duration(doraTeam.DaysBetweenDeployments)
	// }
	// logger.Sugar().Infof("Generating %v deployment events at %v intervals for %s Dora Performance Level", deploymentEvents, interval, doraTeam.Level)

	// createdAtTimestamps := generateTimestamps(deploymentEvents, interval)
	logger.Sugar().Infof("Dora Performance Level: %s, Created %v Timestamps\n", doraTeam.Level, len(successfulDeployTimestamps))

	requestWg := sync.WaitGroup{}
	for _, ts := range successfulDeployTimestamps {
		newPayload := stringReplaceFirst(payload, "CREATED_AT_UPDATE_ME", ts.Format("2006-01-02T15:04:05Z"))
		requestWg.Add(1)
		go func(payload []byte) {
			defer requestWg.Done()
			sendPayload(client, otelWebhookUrl, payload)
		}(newPayload)
	}
	requestWg.Wait()
}
