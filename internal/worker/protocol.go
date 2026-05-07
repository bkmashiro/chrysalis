// Package worker defines the JSON wire types for the Go ↔ Python worker protocol.
package worker

// ArgKind identifies how an argument is encoded on the wire.
type ArgKind string

const (
	KindScalar  ArgKind = "scalar"
	KindList    ArgKind = "list"
	KindDict    ArgKind = "dict"
	KindNDArray ArgKind = "ndarray"
	KindCallback ArgKind = "callback"
)

// Arg represents a single argument or return value on the wire.
type Arg struct {
	Type     ArgKind     `json:"type"`
	Value    interface{} `json:"value,omitempty"`
	// ndarray fields
	ShmHandle int         `json:"shm_handle,omitempty"`
	Shape     []int       `json:"shape,omitempty"`
	DType     string      `json:"dtype,omitempty"`
	// callback field
	CbID      int         `json:"cb_id,omitempty"`
}

// Message is the top-level envelope for all wire messages.
type Message struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`

	// call / probe
	Func   string `json:"func,omitempty"`
	Target string `json:"target,omitempty"`
	Args   []Arg  `json:"args,omitempty"`
	Kwargs map[string]Arg `json:"kwargs,omitempty"`

	// result
	Value *Arg `json:"value,omitempty"`

	// probe_result
	IsModule   bool `json:"is_module,omitempty"`
	IsCallable bool `json:"is_callable,omitempty"`

	// error
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`

	// callback
	CbID int   `json:"cb_id,omitempty"`

	// ready / pong / shutdown_ack — no extra fields needed
}
