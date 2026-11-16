package pluginhost

import (
	"bytes"
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

// HasReasoningReplayPlugins reports whether any active, unfused plugin offers
// reasoning replay, so executors can skip materializing the request otherwise.
func (h *Host) HasReasoningReplayPlugins() bool {
	if h == nil {
		return false
	}
	for _, record := range h.activeRecords() {
		if record.plugin.Capabilities.ReasoningReplayPlugin != nil && !h.isPluginFused(record.id) && h.recordCurrent(record) {
			return true
		}
	}
	return false
}

func (h *Host) PrepareReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayPrepareRequest) pluginapi.ReasoningReplayPrepareResponse {
	if h == nil || strings.TrimSpace(req.RequestID) == "" {
		return pluginapi.ReasoningReplayPrepareResponse{}
	}
	for _, record := range h.activeRecords() {
		plugin := record.plugin.Capabilities.ReasoningReplayPlugin
		if plugin == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
			continue
		}
		resp, ok := h.callReasoningReplayPrepare(ctx, record, plugin, req)
		if !ok || !resp.Handled {
			continue
		}
		h.reasoningReplayMu.Lock()
		h.reasoningReplayOwners[req.RequestID] = record
		h.reasoningReplayMu.Unlock()
		return resp
	}
	return pluginapi.ReasoningReplayPrepareResponse{}
}

func (h *Host) ObserveReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayObservation) {
	record, plugin, ok := h.reasoningReplayOwner(req.RequestID, false)
	if !ok {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "ReasoningReplayPlugin.ObserveReasoningReplay", recovered)
		}
	}()
	req.Body = bytes.Clone(req.Body)
	if errObserve := plugin.ObserveReasoningReplay(ctx, req); errObserve != nil {
		log.Warnf("pluginhost: reasoning replay observer %s failed: %v", record.id, errObserve)
	}
}

func (h *Host) CompleteReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayCompletion) {
	record, plugin, ok := h.reasoningReplayOwner(req.RequestID, true)
	if !ok {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "ReasoningReplayPlugin.CompleteReasoningReplay", recovered)
		}
	}()
	if errComplete := plugin.CompleteReasoningReplay(ctx, req); errComplete != nil {
		log.Warnf("pluginhost: reasoning replay completion %s failed: %v", record.id, errComplete)
	}
}

func (h *Host) callReasoningReplayPrepare(ctx context.Context, record capabilityRecord, plugin pluginapi.ReasoningReplayPlugin, req pluginapi.ReasoningReplayPrepareRequest) (out pluginapi.ReasoningReplayPrepareResponse, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "ReasoningReplayPlugin.PrepareReasoningReplay", recovered)
			out = pluginapi.ReasoningReplayPrepareResponse{}
			ok = false
		}
	}()
	req.Headers = cloneHeader(req.Headers)
	req.OriginalRequest = bytes.Clone(req.OriginalRequest)
	req.TranslatedRequest = bytes.Clone(req.TranslatedRequest)
	req.Metadata = cloneInterceptorMetadata(req.Metadata)
	resp, errPrepare := plugin.PrepareReasoningReplay(ctx, req)
	if errPrepare != nil {
		log.Warnf("pluginhost: reasoning replay prepare %s failed: %v", record.id, errPrepare)
		return pluginapi.ReasoningReplayPrepareResponse{}, false
	}
	resp.Body = bytes.Clone(resp.Body)
	resp.ResponseHeaders = cloneHeader(resp.ResponseHeaders)
	resp.ResponseBody = bytes.Clone(resp.ResponseBody)
	return resp, true
}

func (h *Host) reasoningReplayOwner(requestID string, remove bool) (capabilityRecord, pluginapi.ReasoningReplayPlugin, bool) {
	if h == nil || strings.TrimSpace(requestID) == "" {
		return capabilityRecord{}, nil, false
	}
	h.reasoningReplayMu.Lock()
	record, found := h.reasoningReplayOwners[requestID]
	if remove {
		delete(h.reasoningReplayOwners, requestID)
	}
	h.reasoningReplayMu.Unlock()
	if !found || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return capabilityRecord{}, nil, false
	}
	plugin := record.plugin.Capabilities.ReasoningReplayPlugin
	return record, plugin, plugin != nil
}
