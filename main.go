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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type BoxList []struct {
	Box struct {
		ID int `json:"id"`
	} `json:"storagebox"`
}

type BoxDetail struct {
	Box Storagebox `json:"storagebox"`
}

type Storagebox struct {
	ID                   int     `json:"id"`
	Login                string  `json:"login"`
	Name                 string  `json:"name"`
	Product              string  `json:"product"`
	Cancelled            bool    `json:"cancelled"`
	Locked               bool    `json:"locked"`
	Location             string  `json:"location"`
	LinkedServer         int     `json:"linked_server"`
	PaidUntil            string  `json:"paid_until"`
	DiskQuota            float64 `json:"disk_quota"`
	DiskUsage            float64 `json:"disk_usage"`
	DiskUsageData        float64 `json:"disk_usage_data"`
	DiskUsageSnapshots   float64 `json:"disk_usage_snapshots"`
	Webdav               bool    `json:"webdav"`
	Samba                bool    `json:"samba"`
	SSH                  bool    `json:"ssh"`
	ExternalReachability bool    `json:"external_reachability"`
	Zfs                  bool    `json:"zfs"`
	Server               string  `json:"server"`
	HostSystem           string  `json:"host_system"`
}

type APIError struct {
	Error struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	} `json:"error"`
}

type Config struct {
	Bucket             string `json:"Bucket"`
	InfluxDBHost       string `json:"InfluxDBHost"`
	InfluxDBApiToken   string `json:"InfluxDBApiToken"`
	Org                string `json:"Org"`
	WebserviceUsername string `json:"WebserviceUsername"`
	WebservicePassword string `json:"WebservicePassword"`
}

type retryableTransport struct {
	transport             http.RoundTripper
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

const apiUrl = "https://robot-ws.your-server.de/storagebox"
const storageBoxApiUrl = "https://robot-ws.your-server.de/storagebox/%d"
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
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
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
	if config.WebserviceUsername == "" {
		log.Fatalln("WebserviceUsername is required")
	}
	if config.WebservicePassword == "" {
		log.Fatalln("WebservicePassword is required")
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
	listReq, _ := http.NewRequest("GET", apiUrl, nil)
	listReq.SetBasicAuth(config.WebserviceUsername, config.WebservicePassword)
	listResp, err := client.Do(listReq)
	if err != nil {
		log.Fatalln("Error trying to get storagebox list: ", err)
	}
	defer listResp.Body.Close()
	listBody, err := io.ReadAll(listResp.Body)
	if err != nil {
		log.Fatalln("Error reading storagebox list data: ", err)
	}
	if listResp.StatusCode != http.StatusOK {
		var apiErr APIError
		err = json.Unmarshal(listBody, &apiErr)
		if err != nil {
			log.Fatalln("Error unmarshalling storagebox list api response: ", err)
		}
		log.Fatalf("Error trying to get storagebox list: %d - %s\n", apiErr.Error.Status, apiErr.Error.Code)
	}

	var boxList BoxList
	err = json.Unmarshal(listBody, &boxList)
	if err != nil {
		log.Fatalln("Error unmarshalling storagebox list data: ", err)
	}

	wg := &sync.WaitGroup{}
	payload := bytes.Buffer{}
	for _, entry := range boxList {
		wg.Add(1)

		go func(payload *bytes.Buffer, apiErrors *atomic.Int64) {
			defer wg.Done()

			boxReq, _ := http.NewRequest("GET", fmt.Sprintf(storageBoxApiUrl, entry.Box.ID), nil)
			boxReq.SetBasicAuth(config.WebserviceUsername, config.WebservicePassword)
			boxResp, err := client.Do(boxReq)
			if err != nil {
				HandleApiError(fmt.Sprintf("Error trying to get storagebox id=%d: ", entry.Box.ID), err, apiErrors)
				return
			}
			defer boxResp.Body.Close()
			boxBody, err := io.ReadAll(boxResp.Body)
			if err != nil {
				HandleApiError(fmt.Sprintf("Error reading storagebox id=%d data: ", entry.Box.ID), err, apiErrors)
				return
			}
			if boxResp.StatusCode != http.StatusOK {
				var apiErr APIError
				err = json.Unmarshal(boxBody, &apiErr)
				if err != nil {
					HandleApiError(fmt.Sprintf("Error unmarshalling storagebox id=%d response data: ", entry.Box.ID), err, apiErrors)
					return
				}
				HandleApiError(fmt.Sprintf("Error trying to get storagebox id=%d: %d, - %s\n", entry.Box.ID, apiErr.Error.Status, apiErr.Error.Code), err, apiErrors)
				return
			}
			var details BoxDetail
			err = json.Unmarshal(boxBody, &details)
			if err != nil {
				HandleApiError(fmt.Sprintf("Error unmarshalling storagebox id=%d api response data: %s", entry.Box.ID, string(boxBody)), err, apiErrors)
				return
			}
			timestamp := time.Now()
			paidUntil, err := time.Parse("2006-01-02", details.Box.PaidUntil)
			if err != nil {
				HandleApiError(fmt.Sprintf("Error parsing storagebox id=%d timestamp:", entry.Box.ID), err, apiErrors)
				return
			}

			influxLine := fmt.Sprintf("storagebox_stats,id=%d,name=%s,product=%s,cancelled=%t,location=%s,linked_server=%d,samba=%t,ssh=%t,external_reachability=%t,server=%s,host=%s,webdav=%t,zfs=%t size=%.0f,used=%.0f,used_data=%.0f,used_snapshot=%.0f,paid_until=%v %v\n",
				entry.Box.ID,
				escapeTagValue(details.Box.Name),
				details.Box.Product,
				details.Box.Cancelled,
				details.Box.Location,
				details.Box.LinkedServer,
				details.Box.Samba,
				details.Box.SSH,
				details.Box.ExternalReachability,
				details.Box.Server,
				details.Box.HostSystem,
				details.Box.Webdav,
				details.Box.Zfs,
				details.Box.DiskQuota,
				details.Box.DiskUsage,
				details.Box.DiskUsageData,
				details.Box.DiskUsageSnapshots,
				paidUntil.Unix(),
				timestamp.Unix(),
			)
			payload.WriteString(influxLine)

		}(&payload, &apiErrors)

		if len(boxList) > 198 {
			log.Println("Sleeping for 30sec to avoid rate limit: https://robot.hetzner.com/doc/webservice/en.html#get-storagebox")
			time.Sleep(30 * time.Second)
		}
	}

	wg.Wait()

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
