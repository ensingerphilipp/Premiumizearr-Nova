package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/directclient"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

func TestDirectTorrentFailureReportsExactGrabAndSurvivesRestart(t *testing.T) {
	for _, arrType := range []config.ArrType{config.Sonarr, config.Radarr, config.Lidarr} {
		t.Run(string(arrType), func(t *testing.T) {
			const hash = "0123456789abcdef0123456789abcdef01234567"
			var reports, lookups atomic.Int32
			arrServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "arr-key" {
					t.Error("failure reporter omitted the *arr API key")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/history"):
					lookups.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"totalRecords":4,"records":[
					{"id":40,"eventType":"grabbed","sourceTitle":"Release","downloadId":%q},
					{"id":45,"eventType":"grabbed","sourceTitle":"Different visible name","downloadId":%q},
					{"id":99,"eventType":"grabbed","sourceTitle":"Release","downloadId":"other-torrent"},
					{"id":100,"eventType":"downloadFailed","sourceTitle":"Release","downloadId":%q}]}`, hash, strings.ToUpper(hash), hash)
				case (strings.HasSuffix(r.URL.Path, "/history/failed/45") ||
					arrType == config.Lidarr && strings.HasSuffix(r.URL.Path, "/history/failed")) && r.Method == http.MethodPost:
					if arrType == config.Lidarr {
						body, err := io.ReadAll(r.Body)
						if err != nil || string(body) != "id=45" {
							t.Errorf("unexpected Lidarr failed-history payload: %q (%v)", body, err)
						}
					}
					if reports.Add(1) == 1 {
						http.Error(w, "temporary outage", http.StatusServiceUnavailable)
						return
					}
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected *arr request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer arrServer.Close()
			var transferStatus atomic.Value
			transferStatus.Store("error")
			pmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/account/info":
					fmt.Fprint(w, `{"status":"success","limit_used":0}`)
				case "/api/folder/list":
					fmt.Fprint(w, `{"status":"success","content":[]}`)
				case "/api/folder/create":
					fmt.Fprint(w, `{"status":"success","id":"folder-1"}`)
				case "/api/transfer/create":
					fmt.Fprint(w, `{"status":"success","id":"transfer-1"}`)
				case "/api/transfer/list":
					fmt.Fprintf(w, `{"status":"success","transfers":[{"id":"transfer-1","status":%q,"message":"invalid source"}]}`, transferStatus.Load())
				default:
					t.Errorf("unexpected Premiumize request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer pmServer.Close()
			cfg := &config.Config{TransferDirectory: "arrDownloads", SimultaneousDownloads: 1,
				Arrs: []config.ArrConfig{{Type: arrType, URL: arrServer.URL, APIKey: "arr-key"}}}
			am := ArrsManagerService{}.New()
			am.Init(cfg)
			am.Start()
			pm := premiumizeme.NewPremiumizemeClient("pm-key")
			pm.APIBaseURL = pmServer.URL + "/api/"
			pm.HTTPClient = pmServer.Client()
			stateDir := t.TempDir()
			manager, err := directclient.NewManager(&pm, cfg, stateDir)
			if err != nil {
				t.Fatal(err)
			}
			manager.SetTorrentFailureReporter(am.ReportDirectTorrentFailure)
			if err := manager.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+hash+"&dn=Release", "tv"); err != nil {
				t.Fatal(err)
			}
			manager.PollOnce(context.Background())
			if reports.Load() != 1 {
				t.Fatal("failed torrent did not attempt to mark its exact grabbed record")
			}
			manager.PollOnce(context.Background())
			if reports.Load() != 2 {
				t.Fatal("failed report was not retried")
			}
			// Finished cloud status cannot resurrect a terminal local failure.
			transferStatus.Store("finished")
			manager.PollOnce(context.Background())
			restarted, err := directclient.NewManager(&pm, cfg, stateDir)
			if err != nil {
				t.Fatal(err)
			}
			restarted.SetTorrentFailureReporter(am.ReportDirectTorrentFailure)
			restarted.PollOnce(context.Background())
			if reports.Load() != 2 || lookups.Load() != 2 {
				t.Fatalf("acknowledged report repeated across polls/restart: reports=%d lookups=%d", reports.Load(), lookups.Load())
			}
			if jobs := restarted.ListTorrents("tv"); len(jobs) != 1 || jobs[0].State != "failed" {
				t.Fatalf("terminal failure was lost: %#v", jobs)
			}
		})
	}
}
