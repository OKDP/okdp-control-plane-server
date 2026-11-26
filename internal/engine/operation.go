package engine

import (
	"errors"
	"strings"
)

// Outcome is what became of a write.
type Outcome string

const (
	// OutcomeAccepted: the engine stored the desired state. Nothing says it is applied yet.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeRejected: nothing was stored, Problems say why.
	OutcomeRejected Outcome = "rejected"
	// OutcomeUnknown: the write may or may not have been stored (timeout, push
	// without answer). The caller reads the state again instead of assuming.
	OutcomeUnknown Outcome = "unknown"
)

// Problem codes.
const (
	CodeInvalidParameters   = "invalid-parameters"
	CodeConflict            = "conflict"
	CodeInstanceExists      = "instance-exists"
	CodeInvalidCatalogEntry = "invalid-catalog-entry"
	CodeRejected            = "rejected"
)

// Problem is one reason a write was rejected.
type Problem struct {
	Code    string
	Field   string
	Message string
}

// Operation is the answer to a write.
type Operation struct {
	Outcome Outcome
	// DesiredRevision is the revision the write produced, empty when rejected
	// or when the engine has none.
	DesiredRevision string
	// Problems is set when Outcome is OutcomeRejected.
	Problems []Problem
	// Message explains an OutcomeUnknown, when the engine knows more.
	Message string
	// Instance is the instance as the write left it, when the engine can tell
	// without waiting. Nil for catalog writes, and allowed to be nil for an
	// engine that only learns the result later.
	Instance *InstanceView
}

// WriteOptions qualifies a write.
type WriteOptions struct {
	// ExpectedRevision, when set, makes the write fail with CodeConflict unless
	// the stored desired state is still at that revision.
	ExpectedRevision string
	// IdempotencyKey, when set, makes a replayed create answer the operation of
	// the first one. Reusing a key for a different request is a CodeConflict.
	// Engines that do not support it ignore it, see the engine documentation.
	IdempotencyKey string
}

// ErrNotFound is wrapped by every error about an instance that does not exist.
var ErrNotFound = errors.New("not found")

// ErrOutcomeUnknown is matched by the error the core returns for an OutcomeUnknown write.
var ErrOutcomeUnknown = errors.New("the outcome of the write is unknown, read the instance again")

// ErrNamespaceProvisioning is returned in git mode when a Kubernetes Secret
// cannot be created yet because the project Namespace has not been applied to
// the cluster by the sync tool.
var ErrNamespaceProvisioning = errors.New("project namespace is still being provisioned")

// Rejected builds a rejected operation with one problem.
func Rejected(code, message string) Operation {
	return Operation{Outcome: OutcomeRejected, Problems: []Problem{{Code: code, Message: message}}}
}

// UnknownError carries an unknown outcome as an error. It matches ErrOutcomeUnknown.
type UnknownError struct {
	Operation Operation
}

func (e *UnknownError) Error() string {
	if e.Operation.Message != "" {
		return e.Operation.Message
	}
	return ErrOutcomeUnknown.Error()
}

func (e *UnknownError) Is(target error) bool {
	return target == ErrOutcomeUnknown
}

// RejectedError carries a rejected operation as an error.
type RejectedError struct {
	Operation Operation
}

func (e *RejectedError) Error() string {
	messages := make([]string, 0, len(e.Operation.Problems))
	for _, p := range e.Operation.Problems {
		messages = append(messages, p.Message)
	}
	return strings.Join(messages, "; ")
}

// HasCode reports whether one of the problems carries code.
func (e *RejectedError) HasCode(code string) bool {
	for _, p := range e.Operation.Problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

// IsRejected reports whether err is a RejectedError carrying code.
func IsRejected(err error, code string) bool {
	var rejected *RejectedError
	return errors.As(err, &rejected) && rejected.HasCode(code)
}
