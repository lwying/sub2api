package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const keyBillingInfoSchemaVersion = 1

type keyBillingInfoResponse struct {
	Object                  string    `json:"object"`
	SchemaVersion           int       `json:"schema_version"`
	BillingScope            string    `json:"billing_scope"`
	GroupRateMultiplier     float64   `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64  `json:"user_rate_multiplier,omitempty"`
	ResolvedRateMultiplier  float64   `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool      `json:"peak_rate_enabled"`
	PeakStart               *string   `json:"peak_start,omitempty"`
	PeakEnd                 *string   `json:"peak_end,omitempty"`
	PeakRateMultiplier      *float64  `json:"peak_rate_multiplier,omitempty"`
	AppliedPeakMultiplier   *float64  `json:"applied_peak_multiplier,omitempty"`
	EffectiveRateMultiplier float64   `json:"effective_rate_multiplier"`
	Timezone                *string   `json:"timezone,omitempty"`
	ObservedAt              time.Time `json:"observed_at"`
}

// KeyBillingInfo returns the token billing multiplier effective for the authenticated API key.
// GET /v1/sub2api/billing
func (h *GatewayHandler) KeyBillingInfo(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if h.cfg != nil && h.cfg.RunMode == config.RunModeSimple {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Billing information is not supported in simple mode")
		return
	}
	if h.keyBillingSnapshot != nil && h.settingService != nil {
		settings, err := h.settingService.GetKeyBillingSnapshotSettings(c.Request.Context())
		if err != nil {
			h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Billing information is temporarily unavailable")
			return
		}
		if settings.Enabled {
			h.respondWithKeyBillingSnapshot(c, apiKey, settings.Generation)
			return
		}
	}
	if apiKey.GroupID == nil {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	if apiKey.Group == nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Billing information is unavailable")
		return
	}
	resolvedRate, ok := h.resolveKeyBillingRate(c, apiKey)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Billing information is unavailable")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, buildKeyBillingInfo(apiKey, resolvedRate, timezone.Now()))
}

func (h *GatewayHandler) respondWithKeyBillingSnapshot(c *gin.Context, apiKey *service.APIKey, generation string) {
	credential := keyBillingPresentedCredential(c)
	identity, err := h.keyBillingSnapshot.ReadAuthoritativeIdentity(c.Request.Context(), apiKey.ID, credential)
	if errors.Is(err, service.ErrKeyBillingSnapshotUnboundKey) {
		// The authoritative row binds this key to no group. Answer exactly like the
		// live path does for the same condition; a stale cached apiKey.GroupID must
		// never turn this into a server error.
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	if err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Billing information is temporarily unavailable")
		return
	}
	if len(identity.IPWhitelist) > 0 || len(identity.IPBlacklist) > 0 {
		clientIP := ip.GetSecurityClientIP(c, h.cfg.TrustForwardedIPForAPIKeyACL())
		allowed, _ := ip.CheckIPRestriction(clientIP, identity.IPWhitelist, identity.IPBlacklist)
		if !allowed {
			h.errorResponse(c, http.StatusForbidden, "permission_error", "Access denied")
			return
		}
	}
	payload, enabled, err := h.keyBillingSnapshot.GetOrCreate(c.Request.Context(), identity.Binding, func(ctx context.Context) ([]byte, error) {
		resolved, _, resolveErr := h.keyBillingSnapshot.ResolveKeyBillingRate(ctx, identity.Binding.UserID, identity.Binding.GroupID, identity.GroupRate)
		if resolveErr != nil {
			return nil, resolveErr
		}
		groupID := identity.Binding.GroupID
		current := &service.APIKey{
			UserID:  identity.Binding.UserID,
			GroupID: &groupID,
			Group: &service.Group{
				ID: groupID, Platform: identity.GroupPlatform,
				SubscriptionType: identity.GroupSubscriptionType,
				RateMultiplier:   identity.GroupRate,
				PeakRateEnabled:  identity.PeakRateEnabled,
				PeakStart:        identity.PeakStart, PeakEnd: identity.PeakEnd,
				PeakRateMultiplier: identity.PeakRateMultiplier,
			},
		}
		observedAt, clockErr := h.keyBillingSnapshot.ReadSharedTime(ctx)
		if clockErr != nil {
			return nil, clockErr
		}
		return json.Marshal(buildKeyBillingInfo(current, resolved, observedAt))
	})
	if err != nil || !enabled || len(payload) == 0 {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Billing information is temporarily unavailable")
		return
	}
	latest, latestErr := h.keyBillingSnapshot.ReadAuthoritativeIdentity(c.Request.Context(), apiKey.ID, credential)
	if errors.Is(latestErr, service.ErrKeyBillingSnapshotUnboundKey) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	if latestErr != nil || latest.Binding != identity.Binding {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Billing information is temporarily unavailable")
		return
	}
	if len(latest.IPWhitelist) > 0 || len(latest.IPBlacklist) > 0 {
		clientIP := ip.GetSecurityClientIP(c, h.cfg.TrustForwardedIPForAPIKeyACL())
		allowed, _ := ip.CheckIPRestriction(clientIP, latest.IPWhitelist, latest.IPBlacklist)
		if !allowed {
			h.errorResponse(c, http.StatusForbidden, "permission_error", "Access denied")
			return
		}
	}
	currentSettings, settingsErr := h.settingService.GetKeyBillingSnapshotSettings(c.Request.Context())
	if settingsErr != nil || !currentSettings.Enabled || currentSettings.Generation != generation ||
		currentSettings.MaxStaleHours < service.MinKeyBillingSnapshotMaxStaleHours ||
		currentSettings.MaxStaleHours > service.MaxKeyBillingSnapshotMaxStaleHours {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Billing information is temporarily unavailable")
		return
	}
	var declaration struct {
		ObservedAt time.Time `json:"observed_at"`
	}
	decodeErr := json.Unmarshal(payload, &declaration)
	sharedNow, clockErr := h.keyBillingSnapshot.ReadSharedTime(c.Request.Context())
	age := sharedNow.Sub(declaration.ObservedAt)
	if decodeErr != nil || clockErr != nil || declaration.ObservedAt.IsZero() ||
		age < 0 || age > time.Duration(currentSettings.MaxStaleHours)*time.Hour {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Billing information is temporarily unavailable")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}

func keyBillingPresentedCredential(c *gin.Context) string {
	authHeader := c.GetHeader("Authorization")
	if parts := strings.SplitN(authHeader, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		if key := strings.TrimSpace(parts[1]); key != "" {
			return key
		}
	}
	if key := c.GetHeader("x-api-key"); key != "" {
		return key
	}
	return c.GetHeader("x-goog-api-key")
}

func (h *GatewayHandler) resolveKeyBillingRate(c *gin.Context, apiKey *service.APIKey) (float64, bool) {
	groupRate := apiKey.Group.RateMultiplier
	switch apiKey.Group.Platform {
	case service.PlatformOpenAI, service.PlatformGrok:
		if h.openAIGatewayService == nil {
			return 0, false
		}
		return h.openAIGatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), apiKey.UserID, *apiKey.GroupID, groupRate), true
	default:
		if h.gatewayService == nil {
			return 0, false
		}
		return h.gatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), apiKey.UserID, *apiKey.GroupID, groupRate), true
	}
}

func buildKeyBillingInfo(apiKey *service.APIKey, resolvedRate float64, now time.Time) keyBillingInfoResponse {
	groupRate := apiKey.Group.RateMultiplier
	var userRate *float64
	if resolvedRate != groupRate {
		userRate = &resolvedRate
	}
	appliedPeak := apiKey.Group.PeakMultiplierAt(now)

	response := keyBillingInfoResponse{
		Object:                  "sub2api.key_billing",
		SchemaVersion:           keyBillingInfoSchemaVersion,
		BillingScope:            "token",
		GroupRateMultiplier:     groupRate,
		UserRateMultiplier:      userRate,
		ResolvedRateMultiplier:  resolvedRate,
		PeakRateEnabled:         apiKey.Group.PeakRateEnabled,
		EffectiveRateMultiplier: resolvedRate * appliedPeak,
		ObservedAt:              now.UTC(),
	}
	if apiKey.Group.PeakRateEnabled {
		response.PeakStart = &apiKey.Group.PeakStart
		response.PeakEnd = &apiKey.Group.PeakEnd
		response.PeakRateMultiplier = &apiKey.Group.PeakRateMultiplier
		response.AppliedPeakMultiplier = &appliedPeak
		tz := timezone.Location().String()
		response.Timezone = &tz
	}
	return response
}
