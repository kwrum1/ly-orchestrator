package service

import (
	"context"
	"strconv"
	"strings"
)

// ApplyIdentity is the caller transaction identity for one service apply.
// It is propagated to helpers such as vpp-apply so their receipt can be bound
// to the control-plane transaction instead of inventing a private one.
type ApplyIdentity struct {
	TransactionID     string
	DesiredGeneration int64
	DesiredHash       string
}

type applyIdentityContextKey struct{}

func withApplyIdentity(ctx context.Context, identity ApplyIdentity) context.Context {
	existing := applyIdentityFromContext(ctx)
	if identity.TransactionID == "" {
		identity.TransactionID = existing.TransactionID
	}
	if identity.DesiredGeneration == 0 {
		identity.DesiredGeneration = existing.DesiredGeneration
	}
	if identity.DesiredHash == "" {
		identity.DesiredHash = existing.DesiredHash
	}
	return context.WithValue(ctx, applyIdentityContextKey{}, identity)
}

func withTransactionID(ctx context.Context, transactionID string) context.Context {
	return withApplyIdentity(ctx, ApplyIdentity{TransactionID: transactionID})
}

func transactionIDFromContext(ctx context.Context) string {
	return applyIdentityFromContext(ctx).TransactionID
}

func applyIdentityFromContext(ctx context.Context) ApplyIdentity {
	if ctx == nil {
		return ApplyIdentity{}
	}
	identity, _ := ctx.Value(applyIdentityContextKey{}).(ApplyIdentity)
	return identity
}

// vppApplyEnvironment is the caller identity passed to the vpp-apply helper.
// The helper must echo these values in its receipt; a receipt that invents its
// own transaction id or reports generation zero can no longer advance the
// control-plane generation.
func vppApplyEnvironment(identity ApplyIdentity) []string {
	environment := make([]string, 0, 3)
	if transactionID := strings.TrimSpace(identity.TransactionID); transactionID != "" {
		environment = append(environment, "LY_ROUTE_APPLY_TRANSACTION_ID="+transactionID)
	}
	if identity.DesiredGeneration > 0 {
		generation := strconv.FormatInt(identity.DesiredGeneration, 10)
		environment = append(environment, "LY_ROUTE_DESIRED_GENERATION="+generation)
		environment = append(environment, "LY_ROUTE_APPLIED_GENERATION="+generation)
	}
	if hash := strings.TrimSpace(identity.DesiredHash); hash != "" {
		environment = append(environment, "LY_ROUTE_DESIRED_HASH="+hash)
	}
	return environment
}
