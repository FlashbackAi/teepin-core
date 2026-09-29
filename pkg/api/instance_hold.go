// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/cluster"
	"github.com/FlashbackAi/teepin-core/pkg/compute"
	"github.com/FlashbackAi/teepin-core/pkg/gpu"
	"github.com/FlashbackAi/teepin-core/pkg/models"
)

// stoppedMessage is what a customer sees on an instance stopped for lack of
// credit. The deletion date comes from the storage hold (see GetStorageHold).
const stoppedMessage = "Stopped because your account ran out of credit. Your disk is kept for a limited time: add credit, then start the instance to get it back."

// WithSpecVault enables storing instances' launch specs (encrypted), which
// is what lets an instance with a disk be stopped-and-held at zero credit
// and started again. Without a vault such instances cannot be held, and the
// credit enforcer reports them instead of stopping them.
func (s *Server) WithSpecVault(v *compute.SpecVault) *Server {
	s.specVault = v
	return s
}

// persistLaunchSpec stores the sealed spec of an instance that has a disk.
// Best-effort: a failure leaves the instance running normally but unable to
// be held, which the credit enforcer reports loudly when it matters.
func (s *Server) persistLaunchSpec(ctx context.Context, instanceID string, spec cluster.InstanceSpec) {
	if s.specVault == nil || s.store == nil || spec.StorageGB <= 0 {
		return
	}
	sealed, err := s.specVault.Seal(spec)
	if err != nil {
		log.Printf("WARN: instance %s: could not seal its launch spec (it cannot be held if credit runs out): %v", instanceID, err)
		return
	}
	if err := s.store.SaveLaunchSpec(ctx, instanceID, sealed); err != nil {
		log.Printf("WARN: instance %s: could not store its launch spec (it cannot be held if credit runs out): %v", instanceID, err)
	}
}

// stoppedView is the customer-facing view of a stopped instance. It has no
// pod, so there is no cluster status to build it from.
func (s *Server) stoppedView(ctx context.Context, rec *compute.InstanceRecord) models.Instance {
	st := cluster.InstanceStatus{
		InstanceID: rec.ID,
		Status:     compute.StatusStopped,
		Message:    stoppedMessage,
	}
	return statusToInstance(st, rec, s.vramRate(ctx), s.endpointDomain)
}

// stoppedRecord returns the caller's stopped instance with this id, or nil.
// The record must belong to the caller's project: another tenant's instance
// is indistinguishable from a missing one.
func (s *Server) stoppedRecord(ctx context.Context, projectID uuid.UUID, id string) *compute.InstanceRecord {
	if s.store == nil {
		return nil
	}
	rec, err := s.store.Get(ctx, id)
	if err != nil || rec == nil {
		return nil
	}
	if rec.ProjectID != projectID || rec.Status != compute.StatusStopped || rec.TerminatedAt != nil {
		return nil
	}
	return rec
}

// routeInstance tells the cluster client which provider holds a stopped
// instance's disk, when the client supports it (it forgets on restart).
func (s *Server) routeInstance(rec *compute.InstanceRecord) {
	if r, ok := s.cluster.(cluster.InstanceRouter); ok {
		r.RouteInstance(rec.ID, rec.ProviderID)
	}
}

// deleteStoppedInstance permanently deletes a stopped instance and its disk.
func (s *Server) deleteStoppedInstance(c *gin.Context, rec *compute.InstanceRecord) {
	s.routeInstance(rec)
	// AllTenants: tenancy was checked against the database record above, and
	// a stopped instance has no live status for the scoped check to find.
	if err := s.cluster.DeleteInstance(c.Request.Context(), cluster.AllTenants(), rec.ID); err != nil {
		if errors.Is(err, cluster.ErrClusterUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "The node holding this instance's disk is unreachable right now; retry when it is back online",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := s.store.MarkTerminated(c.Request.Context(), rec.ID); err != nil {
		c.Header("X-Warning", fmt.Sprintf("failed to finalize billing record: %v", err))
	}
	c.JSON(http.StatusOK, gin.H{"message": "instance deleted", "id": rec.ID})
}

// StartInstance handles POST /v1/compute/instances/:id/start: relaunch an
// instance that was stopped when the account ran out of credit, with the
// same id, disk, and endpoint. Needs credit again.
func (s *Server) StartInstance(c *gin.Context) {
	projectID, accountID, ok := s.requireScope(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	rec := s.stoppedRecord(ctx, projectID, c.Param("id"))
	if rec == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no stopped instance with this id"})
		return
	}
	if s.specVault == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "starting stopped instances is not available on this deployment"})
		return
	}

	// Same payment gate as creating an instance.
	if s.gate != nil {
		allowed, reason, err := s.gate.AccountCanProvision(ctx, accountID)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify billing status, please retry"})
			return
		}
		if !allowed {
			c.JSON(http.StatusPaymentRequired, gin.H{"error": reason, "code": "payment_method_required"})
			return
		}
	}
	// And the same minimum runway a launch needs: starting something the
	// account could pay for only briefly would just get it stopped again.
	if s.credit != nil {
		need := creditFloor
		hourly := 0.0
		if rec.GPUVRAMGB > 0 {
			hourly = gpu.PriceForVRAM(rec.GPUVRAMGB, s.vramRate(ctx))
			need = hourly * billing.MinLaunchRunway.Hours()
		}
		okCredit, balance, err := s.credit.CanAfford(ctx, accountID, need)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify billing status, please retry"})
			return
		}
		if !okCredit {
			c.JSON(http.StatusPaymentRequired, gin.H{
				"error": fmt.Sprintf("starting this instance needs at least $%.2f of credit (you have $%.2f); add credit in the console", need, balance),
				"code":  "insufficient_credit",
			})
			return
		}
	}

	sealed, err := s.store.LoadLaunchSpec(ctx, rec.ID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "this instance cannot be started: its launch details were not saved"})
		return
	}
	spec, err := s.specVault.Open(rec.ID, sealed)
	if err != nil {
		log.Printf("api: start %s: %v", rec.ID, err)
		c.JSON(http.StatusConflict, gin.H{"error": "this instance cannot be started: its launch details could not be read"})
		return
	}

	// A GPU slice pinned at creation may have been given to someone else
	// while the instance was stopped; allocate afresh. (A disk on a home node
	// keeps its node - the volume lives there - and home instances have no
	// GPU slice.)
	if rec.GPUVRAMGB > 0 && s.gpuAllocator != nil {
		allocation, err := s.gpuAllocator.AllocateByVRAM(ctx, rec.GPUVRAMGB)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": fmt.Sprintf("GPU allocation failed: %v", err)})
			return
		}
		spec.GPUResource, spec.GPUQuantity = "", 0
		if !allocation.Simulated {
			spec.GPUResource = allocation.ResourceName
			spec.GPUQuantity = allocation.Quantity
		}
		spec.GPUVRAMGB = allocation.AllocatedVRAM
		spec.NodeName = allocation.NodeName
		spec.InstanceType = allocation.InstanceType
	}

	s.routeInstance(rec)
	result, err := s.cluster.CreateInstance(ctx, spec)
	if err != nil {
		switch {
		case errors.Is(err, cluster.ErrClusterUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the node holding this instance's disk is unreachable right now; retry when it is back online"})
		case errors.Is(err, cluster.ErrResourceExhausted):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the GPU was taken by another request; retry"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to start instance: %v", err)})
		}
		return
	}

	changed, err := s.store.MarkStarted(ctx, rec.ID, compute.StartResult{
		PodName:    result.PodName,
		Endpoint:   result.EndpointURL,
		DNSName:    result.DNSName,
		PublicIP:   result.PublicIP,
		TLSEnabled: result.TLSEnabled,
		TLSReady:   result.TLSReady,
	})
	if err != nil || !changed {
		// The pod is up but the record still says stopped: billing would not
		// meter it. Stop it again rather than leave it running unbilled.
		log.Printf("ERROR: instance %s started but its record was not updated (changed=%v err=%v); stopping it again", rec.ID, changed, err)
		if stopper, ok := s.cluster.(cluster.InstanceStopper); ok {
			if stopErr := stopper.StopInstance(ctx, cluster.AllTenants(), rec.ID); stopErr != nil {
				log.Printf("ORPHANED INSTANCE %s: started, record not updated, and could not be stopped - running unbilled, manual cleanup required: %v", rec.ID, stopErr)
			}
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start instance, please retry"})
		return
	}

	updated, _ := s.store.Get(ctx, rec.ID)
	if updated == nil {
		updated = rec
	}
	st := cluster.InstanceStatus{InstanceID: rec.ID, Status: compute.StatusPending}
	c.JSON(http.StatusOK, statusToInstance(st, updated, s.vramRate(ctx), s.endpointDomain))
}
