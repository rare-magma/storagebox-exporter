package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type GetAllStorageBoxesResponse struct {
	StorageBoxes []StorageBox `json:"storage_boxes"`
	Meta         Meta         `json:"meta"`
}

type Meta struct {
	Pagination Pagination `json:"pagination"`
}

type Pagination struct {
	Page         int64 `json:"page"`
	PerPage      int64 `json:"per_page"`
	PreviousPage int64 `json:"previous_page"`
	NextPage     int64 `json:"next_page"`
	LastPage     int64 `json:"last_page"`
	TotalEntries int64 `json:"total_entries"`
}

type StorageBox struct {
	ID             int64          `json:"id"`
	Username       string         `json:"username"`
	Status         string         `json:"status"`
	Name           string         `json:"name"`
	StorageBoxType StorageBoxType `json:"storage_box_type"`
	Location       Location       `json:"location"`
	AccessSettings AccessSettings `json:"access_settings"`
	Server         string         `json:"server"`
	System         string         `json:"system"`
	Stats          Stats          `json:"stats"`
	Labels         Labels         `json:"labels"`
	Protection     Protection     `json:"protection"`
	SnapshotPlan   SnapshotPlan   `json:"snapshot_plan"`
	Created        time.Time      `json:"created"`
}

type AccessSettings struct {
	ReachableExternally bool `json:"reachable_externally"`
	SambaEnabled        bool `json:"samba_enabled"`
	SSHEnabled          bool `json:"ssh_enabled"`
	WebdavEnabled       bool `json:"webdav_enabled"`
	ZfsEnabled          bool `json:"zfs_enabled"`
}

type Labels map[string]string

type Location struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Country     string  `json:"country"`
	City        string  `json:"city"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	NetworkZone string  `json:"network_zone"`
}

type Protection struct {
	Delete bool `json:"delete"`
}

type SnapshotPlan struct {
	MaxSnapshots int64       `json:"max_snapshots"`
	Minute       interface{} `json:"minute"`
	Hour         interface{} `json:"hour"`
	DayOfWeek    interface{} `json:"day_of_week"`
	DayOfMonth   interface{} `json:"day_of_month"`
}

type Stats struct {
	Size          int64 `json:"size"`
	SizeData      int64 `json:"size_data"`
	SizeSnapshots int64 `json:"size_snapshots"`
}

type StorageBoxType struct {
	Name                   string      `json:"name"`
	Description            string      `json:"description"`
	SnapshotLimit          int64       `json:"snapshot_limit"`
	AutomaticSnapshotLimit int64       `json:"automatic_snapshot_limit"`
	SubaccountsLimit       int64       `json:"subaccounts_limit"`
	Size                   int64       `json:"size"`
	Prices                 []Price     `json:"prices"`
	Deprecation            Deprecation `json:"deprecation"`
}

type Deprecation struct {
	UnavailableAfter time.Time `json:"unavailable_after"`
	Announced        time.Time `json:"announced"`
}

type Price struct {
	Location     string      `json:"location"`
	PriceHourly  PriceHourly `json:"price_hourly"`
	PriceMonthly PriceHourly `json:"price_monthly"`
	SetupFee     PriceHourly `json:"setup_fee"`
}

type PriceHourly struct {
	Net   string `json:"net"`
	Gross string `json:"gross"`
}

type APIError struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Details ErrorDetails `json:"details"`
}

type ErrorDetails struct {
	Fields []ErrorField `json:"fields"`
}

type ErrorField struct {
	Name     string   `json:"name"`
	Messages []string `json:"messages"`
}

type Config struct {
	Bucket           string `json:"Bucket"`
	InfluxDBHost     string `json:"InfluxDBHost"`
	InfluxDBApiToken string `json:"InfluxDBApiToken"`
	Org              string `json:"Org"`
	ApiToken         string `json:"ApiToken"`
}

type retryableTransport struct {
	transport             http.RoundTripper
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

const apiUrl = "https://api.hetzner.com/v1/storage_boxes"
const rateLimitDocs = "https://docs.hetzner.cloud/reference/hetzner#rate-limiting"
const retryCount = 3
const stringLimit = 1024

func shouldRetry(err error, resp *http.Response) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

func handleRateLimit(resp *http.Response) {
	remainingStr := resp.Header.Get("RateLimit-Remaining")
	resetStr := resp.Header.Get("RateLimit-Reset")

	if remainingStr == "" {
		return
	}

	remaining, err := strconv.Atoi(remainingStr)
	if err != nil {
		log.Printf("Error parsing RateLimit-Remaining header: %v\n", err)
		return
	}

	if remaining <= 0 {
		resetTimestamp, err := strconv.Atoi(resetStr)
		if err != nil {
			log.Printf("Error parsing RateLimit-Reset header: %v\n", err)
			return
		}

		resetTime := time.Unix(int64(resetTimestamp), 0)
		waitDuration := time.Until(resetTime)

		if waitDuration > 0 {
			log.Printf("Rate limit exceeded. Waiting for %v until reset.\n", waitDuration)
			time.Sleep(waitDuration)
		}
	}
}

func (t *retryableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}
	resp, err := t.transport.RoundTrip(req)
	retries := 0
	for shouldRetry(err, resp) && retries < retryCount {
		backoff := time.Duration(math.Pow(2, float64(retries))) * time.Second
		time.Sleep(backoff)
		if resp != nil && resp.Body != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if req.Body != nil {
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}
		if resp != nil && resp.Status != "" {
			log.Printf("Previous request failed with %s", resp.Status)
		}
		log.Printf("Retry %d of request to: %s", retries+1, req.URL)
		resp, err = t.transport.RoundTrip(req)
		handleRateLimit(resp)
		retries++
	}
	return resp, err
}

func HandleApiError(message string, err error, apiErrors *atomic.Int64) {
	apiErrors.Add(1)
	log.SetOutput(os.Stderr)
	log.Println(message, err)
	log.SetOutput(os.Stdout)
}

func escapeTagValue(value string) string {
	withoutCommas := strings.ReplaceAll(value, ",", `\,`)
	withoutEquals := strings.ReplaceAll(withoutCommas, "=", `\=`)
	escaped := strings.ReplaceAll(withoutEquals, ` `, `\ `)
	runes := []rune(escaped)
	if len(runes) <= stringLimit {
		return escaped
	}
	return string(runes[0:stringLimit-3]) + "..."
}

func writeInfluxLine(payload *bytes.Buffer, response GetAllStorageBoxesResponse) {
	timestamp := time.Now()
	for _, box := range response.StorageBoxes {
		influxLine := fmt.Sprintf("storagebox_stats,id=%d,name=%s,type=%s,status=%s,location=%s,samba=%t,ssh=%t,external_reachability=%t,server=%s,host=%s,webdav=%t,zfs=%t size=%d,used=%d,used_data=%d,used_snapshot=%d %v\n",
			box.ID,
			escapeTagValue(box.Name),
			box.StorageBoxType.Name,
			box.Status,
			box.Location.Name,
			box.AccessSettings.SambaEnabled,
			box.AccessSettings.SSHEnabled,
			box.AccessSettings.ReachableExternally,
			box.Server,
			box.System,
			box.AccessSettings.WebdavEnabled,
			box.AccessSettings.ZfsEnabled,
			box.StorageBoxType.Size,
			box.Stats.Size,
			box.Stats.SizeData,
			box.Stats.SizeSnapshots,
			timestamp.Unix(),
		)
		payload.WriteString(influxLine)
	}
}

func fetchStorageBoxesPage(client *http.Client, apiToken string, apiErrors *atomic.Int64, page int64) GetAllStorageBoxesResponse {
	var details GetAllStorageBoxesResponse
	pageReq, _ := http.NewRequest("GET", fmt.Sprintf(apiUrl+"?per_page=50&page=%d", page), nil)
	pageReq.Header.Add("Authorization", "Bearer "+apiToken)
	pageResp, err := client.Do(pageReq)
	if err != nil {
		HandleApiError(fmt.Sprintf("Error trying to get page=%d: ", page), err, apiErrors)
		return details
	}
	defer pageResp.Body.Close()
	pageBody, err := io.ReadAll(pageResp.Body)
	if err != nil {
		HandleApiError(fmt.Sprintf("Error reading page=%d data: ", page), err, apiErrors)
		return details
	}
	if pageResp.StatusCode != http.StatusOK {
		var apiErr APIError
		err = json.Unmarshal(pageBody, &apiErr)
		if err != nil {
			HandleApiError(fmt.Sprintf("Error unmarshalling page=%d response data: ", page), err, apiErrors)
			return details
		}
		HandleApiError(fmt.Sprintf("Error trying to get page=%d: %s, - %s\n", page, apiErr.Code, apiErr.Message), err, apiErrors)
		return details
	}
	err = json.Unmarshal(pageBody, &details)
	if err != nil {
		HandleApiError(fmt.Sprintf("Error unmarshalling page=%d api response data: %s", page, string(pageBody)), err, apiErrors)
		return details
	}
	return details
}

func main() {
	confFilePath := "storagebox_exporter.json"
	confData, err := os.Open(confFilePath)
	if err != nil {
		log.Fatalln("Error reading config file: ", err)
	}
	defer confData.Close()
	var config Config
	err = json.NewDecoder(confData).Decode(&config)
	if err != nil {
		log.Fatalln("Error reading configuration: ", err)
	}
	if config.ApiToken == "" {
		log.Fatalln("ApiToken is required")
	}
	if config.Bucket == "" {
		log.Fatalln("Bucket is required")
	}
	if config.InfluxDBHost == "" {
		log.Fatalln("InfluxDBHost is required")
	}
	if config.InfluxDBApiToken == "" {
		log.Fatalln("InfluxDBApiToken is required")
	}
	if config.Org == "" {
		log.Fatalln("Org is required")
	}

	transport := &retryableTransport{
		transport:             &http.Transport{},
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	var apiErrors atomic.Int64
	payload := bytes.Buffer{}

	apiResponse := fetchStorageBoxesPage(client, config.ApiToken, &apiErrors, 1)
	writeInfluxLine(&payload, apiResponse)

	lastPage := apiResponse.Meta.Pagination.LastPage

	if lastPage > 1 {
		wg := &sync.WaitGroup{}
		for page := int64(2); page <= lastPage; page++ {
			wg.Add(1)

			go func(payload *bytes.Buffer, apiErrors *atomic.Int64, page int64) {
				defer wg.Done()
				apiResponse := fetchStorageBoxesPage(client, config.ApiToken, apiErrors, page)
				writeInfluxLine(payload, apiResponse)
			}(&payload, &apiErrors, page)

		}

		wg.Wait()
	}

	if len(payload.Bytes()) == 0 {
		log.Fatalln("No data to send")
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(payload.Bytes())
	err = w.Close()
	if err != nil {
		log.Fatalln("Error compressing data: ", err)
	}
	url := fmt.Sprintf("https://%s/api/v2/write?precision=s&org=%s&bucket=%s", config.InfluxDBHost, config.Org, config.Bucket)
	post, _ := http.NewRequest("POST", url, &buf)
	post.Header.Set("Accept", "application/json")
	post.Header.Set("Authorization", "Token "+config.InfluxDBApiToken)
	post.Header.Set("Content-Encoding", "gzip")
	post.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := client.Do(post)
	if err != nil {
		log.Fatalln("Error sending data: ", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalln("Error reading data: ", err)
	}
	if resp.StatusCode != 204 {
		log.Fatal("Error sending data: ", string(body))
	}

	if apiErrors.Load() > 0 {
		log.Fatalf("API errors: %d\n", apiErrors.Load())
	}
}
