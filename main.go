package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
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
)

type DoraPerformanceLevel struct {
	Level string `json:"level"`
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

	doraElitePerformanceLevel = &DoraPerformanceLevel{
		Level: "elite",
	}

	doraHighPerformanceLevel = &DoraPerformanceLevel{
		Level: "high",
	}

	doraMediumPerformanceLevel = &DoraPerformanceLevel{
		Level: "medium",
	}

	doraLowPerformanceLevel = &DoraPerformanceLevel{
		Level: "low",
	}

	rawJSON := []byte(`{
        "level": "debug",
        "encoding": "json",
        "outputPaths": ["stdout"],
        "errorOutputPaths": ["stderr"],
        "initialFields": {"service": "dora-the-explorer"},
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

func stringReplaceFirst(b []byte, pattern string, repl string) []byte {
	re := regexp.MustCompile(fmt.Sprintf(`(.*?)%s(.*)`, regexp.QuoteMeta(pattern)))
	updatedContent := re.ReplaceAll(b, []byte("${1}"+repl+"${2}"))
	return updatedContent
}

func generateTimestamps(count int) []string {
	var timestamps []string
	for i := 1; i <= count; i++ {
		days := time.Duration(i) * 24 * time.Hour
		t := time.Now().Add(-days)
		timestamps = append(timestamps, t.Format("2006-01-02T15:04:05Z"))
	}

	return timestamps
}

func sendPayload(ctx context.Context, wg *sync.WaitGroup, client *http.Client, url string, payload []byte, ts string) {
	defer wg.Done()
	select {
	case <-ctx.Done():
		logger.Sugar().Info("Context is canceled")
		return
	default:
		newPayload := stringReplaceFirst(payload, "CREATED_AT_UPDATE_ME", ts)

		resp, err := client.Post(url, "application/json", bytes.NewBuffer(newPayload))
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
		logger.Sugar().Info("Successfully sent payload")
	}
}

func sendFilePayloadsWithContext(ctx context.Context, url string, filePath string) {
	client := &http.Client{
		Timeout: time.Second * 10,
	}

	file, err := os.Open(filePath)
	if err != nil {
		logger.Sugar().Errorf("Failed to open %s: %v", filePath, err)
		return
	}
	defer file.Close()

	payload, err := io.ReadAll(file)
	if err != nil {
		logger.Sugar().Error("Failed to read deploy.json: ", err)
		return
	}

	timestamps := generateTimestamps(10)
	logger.Sugar().Infof("Timestamps: %v\n", timestamps)

	var wg sync.WaitGroup
	for _, ts := range timestamps {
		wg.Add(1)
		go sendPayload(ctx, &wg, client, url, payload, ts)
	}

	wg.Wait()

	logger.Sugar().Info("Successfully sent deploy.json to the URL")
	logger.Sugar().Info("Successfully pushed logs to Loki")
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	url := os.Getenv("OTEL_WEBHOOK_URL")
	if url == "" {
		logger.Sugar().Error("OTEL_WEBHOOK_URL is not set")
		return
	}

	dataPaths := []string{"./data/issue.json", "./data/deployment.json"}

	for _, path := range dataPaths {
		sendFilePayloadsWithContext(ctx, url, path)
	}
}
