package models

// Operation is how the deployment engine answered a write. Outcome is
// accepted, rejected or unknown. Unknown means the write may or may not have
// been stored: the instance must be read again.
type Operation struct {
	Outcome string `json:"outcome"`
	// DesiredRevision is the revision the write produced, empty when the
	// engine has none.
	DesiredRevision string    `json:"desiredRevision"`
	Problems        []Problem `json:"problems,omitempty"`
}

// Problem is one reason a write was rejected.
type Problem struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// ErrorResponse is the body of a failed request. Error is the human-readable
// message. Code classifies the failure when the server can tell, and
// Operation carries the engine's answer when a write was rejected or its
// outcome is unknown.
type ErrorResponse struct {
	Error     string     `json:"error"`
	Code      string     `json:"code,omitempty"`
	Operation *Operation `json:"operation,omitempty"`
}

// OperationResponse is the body of a write the engine accepted and applies
// later.
type OperationResponse struct {
	Operation *Operation `json:"operation"`
}
