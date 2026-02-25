// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"time"

	"github.com/bureau-foundation/bureau/lib/authorization"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/lib/schema"
)

// sensitiveActionPrefixes defines which action namespaces are considered
// sensitive. Successful authorization of these actions is logged to
// Matrix in addition to denials. This set is defined in code — what
// counts as "sensitive" is a security decision, not an operational knob.
var sensitiveActionPrefixes = []string{
	"credential/provision/",
	schema.ActionInterrupt,
	"fleet/",
	schema.ActionObserveReadWrite,
	schema.ActionGrantApprovePrefix,
}

// isSensitiveAction returns true if the action falls within a sensitive
// namespace. Sensitive actions get audit events posted to Matrix on
// both allow and deny decisions.
func isSensitiveAction(action string) bool {
	for _, prefix := range sensitiveActionPrefixes {
		if action == prefix || strings.HasPrefix(action, prefix) {
			return true
		}
	}
	return false
}

// postAuditDeny posts an audit event for a denied authorization check.
// The event is posted asynchronously — the authorization decision is
// never delayed by Matrix availability.
func (d *Daemon) postAuditDeny(
	actor ref.UserID, action string, target ref.UserID, enforcementPoint string,
	reason authorization.DenyReason,
	matchedAllowance *schema.Allowance,
	matchedAllowanceDenial *schema.AllowanceDenial,
) {
	d.logger.Warn("authorization denied",
		"actor", actor,
		"action", action,
		"target", target,
		"reason", reason.String(),
		"enforcement_point", enforcementPoint,
	)

	content := schema.AuditEventContent{
		Decision:               schema.AuditDeny,
		Actor:                  actor,
		Action:                 action,
		Target:                 target,
		Reason:                 reason.String(),
		EnforcementPoint:       enforcementPoint,
		Machine:                d.machine.UserID(),
		MatchedAllowance:       matchedAllowance,
		MatchedAllowanceDenial: matchedAllowanceDenial,
	}
	d.postAuditEventAsync(content)
}

// postAuditAllow posts an audit event for a successful sensitive
// authorization check. Only called for actions in the sensitive set.
func (d *Daemon) postAuditAllow(
	actor ref.UserID, action string, target ref.UserID, enforcementPoint string,
	matchedAllowance *schema.Allowance,
) {
	d.logger.Info("authorization allowed (sensitive)",
		"actor", actor,
		"action", action,
		"target", target,
		"enforcement_point", enforcementPoint,
	)

	content := schema.AuditEventContent{
		Decision:         schema.AuditAllow,
		Actor:            actor,
		Action:           action,
		Target:           target,
		EnforcementPoint: enforcementPoint,
		Machine:          d.machine.UserID(),
		MatchedAllowance: matchedAllowance,
	}
	d.postAuditEventAsync(content)
}

// postAuditEventAsync posts an m.bureau.audit timeline event to the
// config room in a goroutine. If the post fails, the event is already
// logged locally via slog in the calling function.
//
// Skips the Matrix post when there is no session (unit tests without a
// homeserver) or no config room ID.
func (d *Daemon) postAuditEventAsync(content schema.AuditEventContent) {
	if d.session == nil || d.configRoomID.IsZero() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := d.sendEventRetry(ctx, d.configRoomID, schema.EventTypeAudit, content); err != nil {
			d.logger.Warn("failed to post audit event to Matrix",
				"error", err,
				"decision", string(content.Decision),
				"actor", content.Actor,
				"action", content.Action,
			)
		}
	}()
}
