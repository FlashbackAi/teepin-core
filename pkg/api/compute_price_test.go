// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/models"
	"github.com/FlashbackAi/teepin-core/pkg/teepinbuild"
)

// cpuPricing quotes CPU at $0.002 per core-hour, $0.001 per GB-hour of memory
// and $0.0001 per GB-hour of disk, standing in for billing.Service. loads counts
// how many times the "rates" were read, to prove a list reads them once.
type cpuPricing struct{ loads *int }

func (cpuPricing) VRAMPricePerGBHour(context.Context) float64 { return 0.1 }
func (p cpuPricing) ComputeHourlyPricer(context.Context) func(gpuVRAMGB, cpuUnits, memoryGB, storageGB int) float64 {
	if p.loads != nil {
		*p.loads++
	}
	return func(gpuVRAMGB, cpuUnits, memoryGB, storageGB int) float64 {
		return float64(cpuUnits)*0.002 + float64(memoryGB)*0.001 + float64(storageGB)*0.0001
	}
}

// A CPU app reports what it will actually cost per hour (cores, memory and
// disk), not the GPU-only field's $0, so "Cost: $0.0000/hour" no longer reaches
// a customer for an app that bills.
func TestCreateInstance_CPUInstanceReportsItsHourlyPrice(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fc := newFakeCluster()
	fc.nextResult = &cluster.InstanceResult{EndpointURL: "https://inst-price01.teepin.com", DNSName: "inst-price01.teepin.com"}
	server := NewServer(fc, nil, compute.NewStore(db), cpuPricing{}, allowGate{})
	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).AddRow(time.Now(), time.Now()))

	w := createInstanceReqBody(server, uuid.New(),
		`{"name":"t","image":"nginx","cpu_units":4,"memory":"8GB","storage_gb":10,"ports":[{"container":8080}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	var got models.Instance
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := 4*0.002 + 8*0.001 + 10*0.0001
	if math.Abs(got.PricePerHour-want) > 1e-12 {
		t.Errorf("price_per_hour = %v, want %v (cores + memory + disk, as billed)", got.PricePerHour, want)
	}
}

// With no pricing available the field stays 0 rather than inventing a number.
func TestCreateInstance_NoPricingLeavesThePriceZero(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	fc := newFakeCluster()
	fc.nextResult = &cluster.InstanceResult{EndpointURL: "https://inst-price02.teepin.com"}
	server := NewServer(fc, nil, compute.NewStore(db), nil, allowGate{})
	mock.ExpectQuery(`INSERT INTO compute\.instances`).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).AddRow(time.Now(), time.Now()))
	w := createInstanceReqBody(server, uuid.New(), `{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB","ports":[{"container":80}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	var got models.Instance
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.PricePerHour != 0 {
		t.Errorf("price_per_hour = %v, want 0", got.PricePerHour)
	}
}

// A redeploy reports the price of the instance's own (unchanged) size.
func TestRedeployBuildInstance_ReportsTheInstancesHourlyPrice(t *testing.T) {
	mock, kStore, cStore := newMockBuildDB(t)
	gw := teepinbuild.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	projectID, sessionID := uuid.New(), uuid.New()
	existingID := "inst-existing1"
	fc := newFakeCluster()
	fc.add(existingID, projectID.String(), compute.StatusRunning)
	server := (&Server{store: cStore, cluster: fc, pricing: cpuPricing{}}).WithBuild(gw)

	// 2 vCPU, 4 GB, 20 GB disk.
	mock.ExpectQuery(`SELECT .+ FROM compute\.instances`).WithArgs(existingID).
		WillReturnRows(instanceRecordRow(existingID, testAccountID, projectID, "build-abc123", "old-image:v1", 2, 4, 20, 80, "https://inst-existing1.teepin.com"))
	mock.ExpectExec(`UPDATE compute\.instances`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.build_workspace_versions`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version = current_workspace_version`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sess := &teepinbuild.Session{ID: sessionID, AccountID: testAccountID, ProjectID: projectID, AppInstanceID: existingID}
	c, w := newRedeployTestContext(projectID)
	server.redeployBuildInstance(context.Background(), c, sessionID, sess, projectID, testAccountID, "new-image:v2",
		[]models.PortMapping{{Container: 80, Protocol: "tcp"}}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	want := 2*0.002 + 4*0.001 + 20*0.0001
	if got, _ := resp["price_per_hour"].(float64); math.Abs(got-want) > 1e-12 {
		t.Errorf("price_per_hour = %v, want %v", resp["price_per_hour"], want)
	}
}

// Instance views show what a CPU instance costs per hour right now: cores,
// memory and disk while it runs, only its disk while stopped, and never touching
// a GPU instance's VRAM price.
func TestCPUQuoter(t *testing.T) {
	loads := 0
	server := &Server{pricing: cpuPricing{loads: &loads}}
	quote := server.cpuQuoter(context.Background())

	view := func(rec *compute.InstanceRecord) models.Instance {
		inst := models.Instance{PricePerHour: 7}
		if rec != nil && rec.GPUVRAMGB == 0 {
			inst.PricePerHour = 0
		}
		quote(&inst, rec)
		return inst
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

	running := &compute.InstanceRecord{Status: compute.StatusRunning, CPUUnits: 2, MemoryGB: 4, StorageGB: 20}
	if got := view(running).PricePerHour; !near(got, 2*0.002+4*0.001+20*0.0001) {
		t.Errorf("running = %v", got)
	}
	pending := &compute.InstanceRecord{Status: compute.StatusPending, CPUUnits: 1, MemoryGB: 1}
	if got := view(pending).PricePerHour; !near(got, 0.003) {
		t.Errorf("starting = %v, want the running price", got)
	}
	stopped := &compute.InstanceRecord{Status: compute.StatusStopped, CPUUnits: 2, MemoryGB: 4, StorageGB: 20}
	if got := view(stopped).PricePerHour; !near(got, 20*0.0001) {
		t.Errorf("stopped = %v, want the disk only", got)
	}
	gpuInst := &compute.InstanceRecord{Status: compute.StatusRunning, GPUVRAMGB: 20, CPUUnits: 8, MemoryGB: 32}
	if got := view(gpuInst).PricePerHour; got != 7 {
		t.Errorf("a GPU instance's VRAM price was overwritten: %v", got)
	}
	if got := view(nil).PricePerHour; got != 7 {
		t.Errorf("an untracked instance was priced: %v", got)
	}
	if loads != 1 {
		t.Errorf("rates read %d times for five instances, want once", loads)
	}
}

// No pricing available: views are left untouched rather than showing a made-up
// price.
func TestCPUQuoter_NoPricingChangesNothing(t *testing.T) {
	quote := (&Server{}).cpuQuoter(context.Background())
	inst := models.Instance{}
	quote(&inst, &compute.InstanceRecord{Status: compute.StatusRunning, CPUUnits: 4, MemoryGB: 8})
	if inst.PricePerHour != 0 {
		t.Errorf("price = %v, want 0", inst.PricePerHour)
	}
}

// A stopped instance in the list/detail view carries its disk-only price.
func TestStoppedView_CarriesTheDiskOnlyPrice(t *testing.T) {
	server := &Server{pricing: cpuPricing{}}
	rec := &compute.InstanceRecord{ID: "inst-stopped1", Status: compute.StatusStopped, CPUUnits: 2, MemoryGB: 4, StorageGB: 50}
	got := server.stoppedView(context.Background(), rec, nil)
	if math.Abs(got.PricePerHour-50*0.0001) > 1e-12 {
		t.Errorf("price_per_hour = %v, want the disk only", got.PricePerHour)
	}
}
