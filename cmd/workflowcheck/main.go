package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"example.com/solo-0021-metrology-assurance-suite/internal/bootstrap"
)

type apiResponse struct {
	Status int
	Body   map[string]any
	Raw    string
}

type harness struct {
	baseURL string
	client  *http.Client
	server  *httptest.Server
}

func main() {
	workflow := flag.String("workflow", "", "workflow id to verify")
	flag.Parse()

	checks := map[string]func() error{
		"register-device":          checkRegisterDevice,
		"open-verification-case":   checkOpenVerificationCase,
		"record-measurement":       checkRecordMeasurement,
		"reopen-verification-case": checkReopenVerificationCase,
		"seal-verification":        checkSealVerification,
		"retire-device":            checkRetireDevice,
	}
	run, ok := checks[*workflow]
	if !ok {
		fmt.Fprintf(flag.CommandLine.Output(), "unknown workflow %q\n", *workflow)
		flag.Usage()
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Printf("workflow %s: failed: %v\n", *workflow, err)
		os.Exit(1)
	}
	fmt.Printf("workflow %s: ok\n", *workflow)
}

func checkRegisterDevice() error {
	h := newHarness()
	defer h.close()

	response, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": " met-1001 ",
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return err
	}
	id, err := nestedString(response.Body, "device", "id")
	if err != nil {
		return err
	}
	tag, err := nestedString(response.Body, "device", "asset_tag")
	if err != nil {
		return err
	}
	if tag != "MET-1001" {
		return fmt.Errorf("asset tag was not normalized: %q", tag)
	}

	read, err := h.request(http.MethodGet, "/devices/"+id, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	readID, err := nestedString(read.Body, "device", "id")
	if err != nil {
		return err
	}
	if readID != id {
		return fmt.Errorf("read returned device %q, expected %q", readID, id)
	}

	duplicate, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": "MET-1001",
		"model":     "Other Meter",
		"room":      "Room B",
		"steward":   "Mira",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(duplicate, http.StatusConflict); err != nil {
		return err
	}
	return expectErrorCode(duplicate, "duplicate_asset_tag")
}

func checkOpenVerificationCase() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1101")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	state, err := nestedString(opened.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("opened case state = %q", state)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}

	second, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	return expectErrorCode(second, "active_case_exists")
}

func checkRecordMeasurement() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1201")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	accepted, err := h.recordMeasurement(caseID, 25.2, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(accepted, http.StatusCreated); err != nil {
		return err
	}
	state, err := nestedString(accepted.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "review_ready" {
		return fmt.Errorf("accepted measurement state = %q", state)
	}
	acceptedValue, err := nestedBool(accepted.Body, "measurements", 0, "accepted")
	if err != nil {
		return err
	}
	if !acceptedValue {
		return errors.New("in-tolerance measurement was not accepted")
	}

	second, err := h.recordMeasurement(caseID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(second, "state_conflict"); err != nil {
		return err
	}

	rejectDeviceID, err := h.createDevice("MET-1202")
	if err != nil {
		return err
	}
	rejectCase, err := h.openCase(rejectDeviceID)
	if err != nil {
		return err
	}
	rejectCaseID, err := nestedString(rejectCase.Body, "case", "id")
	if err != nil {
		return err
	}
	rejected, err := h.recordMeasurement(rejectCaseID, 26.5, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(rejected, http.StatusCreated); err != nil {
		return err
	}
	rejectedState, err := nestedString(rejected.Body, "case", "state")
	if err != nil {
		return err
	}
	if rejectedState != "recheck_required" {
		return fmt.Errorf("out-of-tolerance state = %q", rejectedState)
	}
	rejectedValue, err := nestedBool(rejected.Body, "measurements", 0, "accepted")
	if err != nil {
		return err
	}
	if rejectedValue {
		return errors.New("out-of-tolerance measurement was accepted")
	}
	return nil
}

func checkReopenVerificationCase() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.rejectedCase("MET-1301")
	if err != nil {
		return err
	}
	reopened, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "measurement setup was replaced",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(reopened, http.StatusOK); err != nil {
		return err
	}
	state, err := nestedString(reopened.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("reopened case state = %q", state)
	}
	measurements, err := nestedSlice(reopened.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("reopen changed measurement history: %d entries", len(measurements))
	}

	second, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "duplicate request",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(second, "state_conflict"); err != nil {
		return err
	}

	events, err := h.request(http.MethodGet, "/cases/"+caseID+"/events", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(events, http.StatusOK); err != nil {
		return err
	}
	if !hasEvent(events.Body, "case_reopened") {
		return errors.New("reopen audit event is missing")
	}
	return nil
}

func checkSealVerification() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.readyCase("MET-1401")
	if err != nil {
		return err
	}
	type outcome struct {
		response apiResponse
		err      error
	}
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
				"reviewer": fmt.Sprintf("Reviewer-%d", index+1),
				"summary":  "accepted measurement sealed for review",
			})
			results <- outcome{response: response, err: err}
		}(index)
	}
	wait.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for item := range results {
		if item.err != nil {
			return item.err
		}
		switch item.response.Status {
		case http.StatusCreated:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			return fmt.Errorf("unexpected concurrent seal status %d: %s", item.response.Status, item.response.Raw)
		}
	}
	if successes != 1 || conflicts != 1 {
		return fmt.Errorf("concurrent seal outcomes: %d success, %d conflict", successes, conflicts)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "sealed" {
		return fmt.Errorf("sealed case state = %q", state)
	}
	sealID, err := nestedString(read.Body, "seal", "id")
	if err != nil {
		return err
	}
	if sealID == "" {
		return errors.New("evidence seal is missing")
	}
	return nil
}

func checkRetireDevice() error {
	h := newHarness()
	defer h.close()

	// Devices with a case in every unfinished state cannot be retired.
	awaitingDevice, err := h.createDevice("MET-1501")
	if err != nil {
		return err
	}
	awaitingCase, err := h.openCase(awaitingDevice)
	if err != nil {
		return err
	}
	awaitingCaseID, err := nestedString(awaitingCase.Body, "case", "id")
	if err != nil {
		return err
	}
	blocked, err := h.retireDevice(awaitingDevice, "Lin", "replacement asset arrived")
	if err != nil {
		return err
	}
	if err := expectStatus(blocked, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(blocked, "active_case_exists"); err != nil {
		return err
	}

	recheckDevice, err := h.createDevice("MET-1502")
	if err != nil {
		return err
	}
	recheckCaseID, err := h.rejectedCaseIDFor(recheckDevice)
	if err != nil {
		return err
	}
	blockedRecheck, err := h.retireDevice(recheckDevice, "Lin", "replacement asset arrived")
	if err != nil {
		return err
	}
	if err := expectStatus(blockedRecheck, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(blockedRecheck, "active_case_exists"); err != nil {
		return err
	}

	readyDevice, err := h.createDevice("MET-1503")
	if err != nil {
		return err
	}
	readyCaseID, err := h.readyCaseIDFor(readyDevice)
	if err != nil {
		return err
	}
	sealed, err := h.request(http.MethodPost, "/cases/"+readyCaseID+"/seal", map[string]any{
		"reviewer": "Verifier",
		"summary":  "accepted measurement sealed for review",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(sealed, http.StatusCreated); err != nil {
		return err
	}

	// Retirement requires an actor and a reason.
	missingReason, err := h.retireDevice(readyDevice, "Lin", "   ")
	if err != nil {
		return err
	}
	if err := expectStatus(missingReason, http.StatusBadRequest); err != nil {
		return err
	}
	if err := expectErrorCode(missingReason, "invalid_input"); err != nil {
		return err
	}

	// The rejected retirement left the device active.
	readBlocked, err := h.request(http.MethodGet, "/devices/"+awaitingDevice, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(readBlocked, http.StatusOK); err != nil {
		return err
	}
	blockedStatus, err := nestedString(readBlocked.Body, "device", "status")
	if err != nil {
		return err
	}
	if blockedStatus != "active" {
		return fmt.Errorf("blocked device status = %q, expected active", blockedStatus)
	}

	// The rejected retirement did not touch the unfinished cases.
	awaitingRead, err := h.request(http.MethodGet, "/cases/"+awaitingCaseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(awaitingRead, http.StatusOK); err != nil {
		return err
	}
	awaitingState, err := nestedString(awaitingRead.Body, "case", "state")
	if err != nil {
		return err
	}
	if awaitingState != "awaiting_measurement" {
		return fmt.Errorf("blocked case state = %q, expected awaiting_measurement", awaitingState)
	}
	recheckRead, err := h.request(http.MethodGet, "/cases/"+recheckCaseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(recheckRead, http.StatusOK); err != nil {
		return err
	}
	recheckState, err := nestedString(recheckRead.Body, "case", "state")
	if err != nil {
		return err
	}
	if recheckState != "recheck_required" {
		return fmt.Errorf("blocked case state = %q, expected recheck_required", recheckState)
	}

	// After the last case reaches its terminal state, retirement succeeds once.
	retired, err := h.retireDevice(readyDevice, " Lin ", " replacement asset arrived ")
	if err != nil {
		return err
	}
	if err := expectStatus(retired, http.StatusOK); err != nil {
		return err
	}
	status, err := nestedString(retired.Body, "device", "status")
	if err != nil {
		return err
	}
	if status != "retired" {
		return fmt.Errorf("retired device status = %q", status)
	}
	retiredBy, err := nestedString(retired.Body, "device", "retired_by")
	if err != nil {
		return err
	}
	if retiredBy != "Lin" {
		return fmt.Errorf("retired_by = %q, expected Lin", retiredBy)
	}
	retiredReason, err := nestedString(retired.Body, "device", "retired_reason")
	if err != nil {
		return err
	}
	if retiredReason != "replacement asset arrived" {
		return fmt.Errorf("retired_reason = %q", retiredReason)
	}
	retiredAt, err := nestedString(retired.Body, "device", "retired_at")
	if err != nil {
		return err
	}
	if retiredAt == "" {
		return errors.New("retired_at was not recorded")
	}
	if _, parsed := time.Parse(time.RFC3339, retiredAt); parsed != nil {
		return fmt.Errorf("retired_at is not a timestamp: %q", retiredAt)
	}

	// The detail read keeps the same retirement record.
	read, err := h.request(http.MethodGet, "/devices/"+readyDevice, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	readReason, err := nestedString(read.Body, "device", "retired_reason")
	if err != nil {
		return err
	}
	if readReason != retiredReason {
		return fmt.Errorf("device read changed the retirement reason: %q", readReason)
	}

	// Historical cases, measurements, and case audit events remain readable.
	history, err := h.request(http.MethodGet, "/cases/"+readyCaseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(history, http.StatusOK); err != nil {
		return err
	}
	historyState, err := nestedString(history.Body, "case", "state")
	if err != nil {
		return err
	}
	if historyState != "sealed" {
		return fmt.Errorf("historical case state = %q, expected sealed", historyState)
	}
	measurements, err := nestedSlice(history.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("historical measurements changed: %d entries", len(measurements))
	}
	historyEvents, err := h.request(http.MethodGet, "/cases/"+readyCaseID+"/events", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(historyEvents, http.StatusOK); err != nil {
		return err
	}
	if !hasEvent(historyEvents.Body, "case_sealed") {
		return errors.New("sealed case audit history is missing")
	}

	// A retired device rejects both repeat retirement and new cases.
	again, err := h.retireDevice(readyDevice, "Lin", "duplicate request")
	if err != nil {
		return err
	}
	if err := expectStatus(again, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(again, "state_conflict"); err != nil {
		return err
	}
	newCase, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id": readyDevice,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(newCase, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(newCase, "state_conflict"); err != nil {
		return err
	}

	// Retire racing an open case settles on exactly one outcome.
	raceDevice, err := h.createDevice("MET-1504")
	if err != nil {
		return err
	}
	type outcome struct {
		response apiResponse
		err      error
	}
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		response, err := h.retireDevice(raceDevice, "Lin", "concurrent disposal")
		results <- outcome{response: response, err: err}
	}()
	wait.Add(1)
	go func() {
		defer wait.Done()
		response, err := h.request(http.MethodPost, "/cases", map[string]any{
			"device_id": raceDevice,
			"nominal":   25.0,
			"tolerance": 0.5,
			"steward":   "Qiao",
		})
		results <- outcome{response: response, err: err}
	}()
	wait.Wait()
	close(results)

	retiredWins := 0
	openWins := 0
	for item := range results {
		if item.err != nil {
			return item.err
		}
		switch item.response.Status {
		case http.StatusOK:
			retiredWins++
		case http.StatusCreated:
			openWins++
		case http.StatusConflict:
		default:
			return fmt.Errorf("unexpected concurrent outcome status %d: %s", item.response.Status, item.response.Raw)
		}
	}
	if retiredWins+openWins != 1 {
		return fmt.Errorf("concurrent retire/open outcomes: %d retired, %d opened", retiredWins, openWins)
	}
	final, err := h.request(http.MethodGet, "/devices/"+raceDevice, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(final, http.StatusOK); err != nil {
		return err
	}
	finalStatus, err := nestedString(final.Body, "device", "status")
	if err != nil {
		return err
	}
	switch {
	case retiredWins == 1 && finalStatus == "retired":
	case openWins == 1 && finalStatus == "active":
	default:
		return fmt.Errorf("concurrent final device status = %q (retired wins=%d, open wins=%d)", finalStatus, retiredWins, openWins)
	}
	return nil
}

func newHarness() *harness {
	server := httptest.NewServer(bootstrap.NewHandler())
	return &harness{
		baseURL: server.URL,
		client:  server.Client(),
		server:  server,
	}
}

func (h *harness) close() {
	h.server.Close()
}

func (h *harness) request(method, path string, body any) (apiResponse, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		return apiResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := h.client.Do(request)
	if err != nil {
		return apiResponse{}, err
	}
	defer response.Body.Close()
	rawBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResponse{}, err
	}
	result := apiResponse{Status: response.StatusCode, Raw: string(rawBytes)}
	if len(bytes.TrimSpace(rawBytes)) > 0 {
		if err := json.Unmarshal(rawBytes, &result.Body); err != nil {
			return apiResponse{}, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return result, nil
}

func (h *harness) createDevice(tag string) (string, error) {
	response, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": tag,
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return nestedString(response.Body, "device", "id")
}

func (h *harness) openCase(deviceID string) (apiResponse, error) {
	response, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	})
	if err != nil {
		return apiResponse{}, err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return apiResponse{}, err
	}
	return response, nil
}

func (h *harness) recordMeasurement(caseID string, observed float64, technician string) (apiResponse, error) {
	return h.request(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
		"observed":   observed,
		"technician": technician,
	})
}

func (h *harness) retireDevice(deviceID, actor, reason string) (apiResponse, error) {
	return h.request(http.MethodPost, "/devices/"+deviceID+"/retire", map[string]any{
		"actor":  actor,
		"reason": reason,
	})
}

func (h *harness) rejectedCaseIDFor(deviceID string) (string, error) {
	opened, err := h.openCase(deviceID)
	if err != nil {
		return "", err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return "", err
	}
	response, err := h.recordMeasurement(caseID, 26.5, "Rui")
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return caseID, nil
}

func (h *harness) readyCaseIDFor(deviceID string) (string, error) {
	opened, err := h.openCase(deviceID)
	if err != nil {
		return "", err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return "", err
	}
	response, err := h.recordMeasurement(caseID, 25.2, "Rui")
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return caseID, nil
}

func (h *harness) rejectedCase(tag string) (string, error) {
	deviceID, err := h.createDevice(tag)
	if err != nil {
		return "", err
	}
	return h.rejectedCaseIDFor(deviceID)
}

func (h *harness) readyCase(tag string) (string, error) {
	deviceID, err := h.createDevice(tag)
	if err != nil {
		return "", err
	}
	return h.readyCaseIDFor(deviceID)
}

func expectStatus(response apiResponse, expected int) error {
	if response.Status != expected {
		return fmt.Errorf("status = %d, expected %d: %s", response.Status, expected, response.Raw)
	}
	return nil
}

func expectErrorCode(response apiResponse, expected string) error {
	code, err := nestedString(response.Body, "error", "code")
	if err != nil {
		return err
	}
	if code != expected {
		return fmt.Errorf("error code = %q, expected %q", code, expected)
	}
	return nil
}

func nestedString(root map[string]any, path ...string) (string, error) {
	value, err := nestedValue(root, path...)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", strings.Join(path, "."))
	}
	return text, nil
}

func nestedBool(root map[string]any, path ...any) (bool, error) {
	value, err := nestedAny(root, path...)
	if err != nil {
		return false, err
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%v is not a boolean", path)
	}
	return flag, nil
}

func nestedSlice(root map[string]any, key string) ([]any, error) {
	value, ok := root[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an array", key)
	}
	return items, nil
}

func hasEvent(root map[string]any, kind string) bool {
	items, err := nestedSlice(root, "events")
	if err != nil {
		return false
	}
	for _, item := range items {
		event, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if event["kind"] == kind {
			return true
		}
	}
	return false
}

func nestedValue(root map[string]any, path ...string) (any, error) {
	parts := make([]any, 0, len(path))
	for _, part := range path {
		parts = append(parts, part)
	}
	return nestedAny(root, parts...)
}

func nestedAny(root map[string]any, path ...any) (any, error) {
	var current any = root
	for _, part := range path {
		switch typed := part.(type) {
		case string:
			object, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s is not an object", typed)
			}
			value, ok := object[typed]
			if !ok {
				return nil, fmt.Errorf("missing %s", typed)
			}
			current = value
		case int:
			items, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("index %d is not inside an array", typed)
			}
			if typed < 0 || typed >= len(items) {
				return nil, fmt.Errorf("index %d is out of range", typed)
			}
			current = items[typed]
		default:
			return nil, fmt.Errorf("unsupported path element %T", part)
		}
	}
	return current, nil
}
