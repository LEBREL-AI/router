package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lebrel-router/internal/controlplane"
	"lebrel-router/internal/model"
)

func remoteFor(t *testing.T, handler http.HandlerFunc) *Remote {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := controlplane.New(server.URL, "secret", 5*time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return NewRemote(client)
}

func TestRefusalsKeepTheControlPlanesAnswer(t *testing.T) {
	remote := remoteFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/internal/gateway/reservations/res_closed/settle":
			w.WriteHeader(409)
			fmt.Fprint(w, `{"error":{"code":"settlement_idempotency_conflict","message":"Reservation was settled with different parameters","settled":{"outcome":"usage_missing","actualMicrousd":31313}}}`)
		case "/api/internal/gateway/reservations/res_gone/accepted":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"code":"reservation_not_found","message":"Credit reservation not found"}}`)
		case "/api/internal/gateway/reservations/res_bad/settle":
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"code":"invalid_usage","message":"Usage counters are internally inconsistent"}}`)
		default:
			w.WriteHeader(500)
		}
	})
	_, err := remote.Settle(context.Background(), "res_closed", 9, model.Usage{}, model.SettlementSucceeded, "deepseek/api")
	if !errors.Is(err, ErrReservationConflict) || ErrorCode(err) != "settlement_idempotency_conflict" {
		t.Fatalf("409: %v", err)
	}
	if settled, ok := StoredSettlement(err); !ok || settled.Outcome != model.SettlementUsageMissing || settled.Actual != 31313 {
		t.Fatalf("the settlement the platform names: %+v %t", settled, ok)
	}
	err = remote.MarkAccepted(context.Background(), "res_gone")
	if !errors.Is(err, ErrReservationMissing) || ErrorCode(err) != "reservation_not_found" {
		t.Fatalf("404: %v", err)
	}
	if _, ok := StoredSettlement(err); ok {
		t.Fatal("no settlement named, none read")
	}
	_, err = remote.Settle(context.Background(), "res_bad", 9, model.Usage{}, model.SettlementSucceeded, "deepseek/api")
	var httpError *controlplane.HTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != 400 || httpError.Code != "invalid_usage" {
		t.Fatalf("a 400 must stay readable as the platform's answer: %v", err)
	}
}

func TestDeliverBatchFollowsTheContract(t *testing.T) {
	var received struct {
		Items []map[string]any `json:"items"`
	}
	remote := remoteFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != BatchPath || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("batch call %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		// answers may come in any order; one item is left out
		fmt.Fprint(w, `{"results":[
			{"key":"res_b:settle","status":409,"error":{"code":"settlement_outcome_mismatch","message":"Settlement outcome does not match upstream admission evidence"}},
			{"key":"res_a:accepted","status":200},
			{"key":"res_d:settle","status":200,"reservationId":"res_d","actualMicrousd":0},
			{"key":"res_a:accepted","status":500}
		]}`)
	})
	items := []BatchItem{
		{Kind: BatchAccepted, ReservationID: "res_a"},
		{Kind: BatchSettle, ReservationID: "res_b", Actual: 1250, Usage: model.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedTokens: 40}, Outcome: model.SettlementSucceeded, Provider: "zai/api"},
		{Kind: BatchSettle, ReservationID: "res_c", Actual: 7, Outcome: model.SettlementUsageMissing, Provider: "moonshot/api"},
		{Kind: BatchSettle, ReservationID: "res_d", Actual: 0, Outcome: model.SettlementUpstreamRejected, Provider: "deepseek/api"},
	}
	errs, err := remote.DeliverBatch(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	if len(received.Items) != 4 {
		t.Fatalf("items sent: %v", received.Items)
	}
	accepted, settled := received.Items[0], received.Items[1]
	if accepted["key"] != "res_a:accepted" || accepted["kind"] != "accepted" || accepted["reservationId"] != "res_a" || accepted["usage"] != nil || accepted["actualMicrousd"] != nil {
		t.Fatalf("acceptance item %v", accepted)
	}
	usage, _ := settled["usage"].(map[string]any)
	if settled["key"] != "res_b:settle" || settled["actualMicrousd"] != float64(1250) || settled["outcome"] != "succeeded" || settled["provider"] != "zai/api" ||
		usage["promptTokens"] != float64(100) || usage["completionTokens"] != float64(20) || usage["totalTokens"] != float64(120) || usage["cachedTokens"] != float64(40) {
		t.Fatalf("settlement item %v", settled)
	}
	if zero := received.Items[3]; zero["actualMicrousd"] != float64(0) {
		t.Fatalf("a zero charge is sent as 0, not left out: %v", zero)
	}
	if errs[0] != nil {
		t.Fatalf("the first answer for a key counts: %v", errs[0])
	}
	if !errors.Is(errs[1], ErrReservationConflict) || ErrorCode(errs[1]) != "settlement_outcome_mismatch" {
		t.Fatalf("item refusal %v", errs[1])
	}
	if !errors.Is(errs[2], ErrUnavailable) {
		t.Fatalf("an item left out of the answer is retried: %v", errs[2])
	}
	if errs[3] != nil {
		t.Fatalf("delivered: %v", errs[3])
	}
}

func TestAMissingBatchEndpointSaysSo(t *testing.T) {
	for _, status := range []int{404, 405, 400} {
		remote := remoteFor(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		_, err := remote.DeliverBatch(context.Background(), []BatchItem{{Kind: BatchAccepted, ReservationID: "res_a"}})
		if !errors.Is(err, ErrBatchUnsupported) {
			t.Fatalf("%d: %v", status, err)
		}
	}
	// an answer to the call as a whole says nothing about its items: each is retried, none refused
	for _, status := range []int{503, 409, 402, 308} {
		remote := remoteFor(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":"settlement_outcome_mismatch","message":"whole call"}}`)
		})
		errs, err := remote.DeliverBatch(context.Background(), []BatchItem{{Kind: BatchAccepted, ReservationID: "res_a"}})
		if err != nil || len(errs) != 1 || !errors.Is(errs[0], ErrBatchEnvelope) {
			t.Fatalf("%d: %v %v", status, errs, err)
		}
	}
}

func TestEachBatchItemIsCheckedAlone(t *testing.T) {
	var sent []string
	remote := remoteFor(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []struct {
				Key string `json:"key"`
			} `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := []map[string]any{}
		for _, item := range body.Items {
			sent = append(sent, item.Key)
			results = append(results, map[string]any{"key": item.Key, "status": 200})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	errs, err := remote.DeliverBatch(context.Background(), []BatchItem{
		{Kind: BatchSettle, ReservationID: "res_good", Actual: 3, Outcome: model.SettlementSucceeded},
		{Kind: BatchSettle, ReservationID: "res_bad_outcome", Actual: 3, Outcome: "usage-missing"},
		{Kind: BatchSettle, ReservationID: "res_negative", Actual: -1, Outcome: model.SettlementSucceeded},
		{Kind: "refund", ReservationID: "res_kind"},
		{Kind: BatchAccepted, ReservationID: ""},
		{Kind: BatchAccepted, ReservationID: "res_good"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "res_good:settle" || errs[0] != nil {
		t.Fatalf("only the valid item goes out and is delivered: sent %v, %v", sent, errs[0])
	}
	for i := 1; i <= 4; i++ {
		if !errors.Is(errs[i], ErrInvalidOperation) {
			t.Fatalf("item %d is refused alone as invalid: %v", i, errs[i])
		}
	}
	if !errors.Is(errs[5], ErrUnavailable) || errors.Is(errs[5], ErrInvalidOperation) {
		t.Fatalf("a second operation of one reservation waits for a later call: %v", errs[5])
	}
	if errs, err := remote.DeliverBatch(context.Background(), []BatchItem{{Kind: BatchSettle, ReservationID: "res_x", Outcome: "nope"}}); err != nil || !errors.Is(errs[0], ErrInvalidOperation) || len(sent) != 1 {
		t.Fatalf("a batch with nothing valid sends nothing: %v %v, sent %v", errs, err, sent)
	}
}
