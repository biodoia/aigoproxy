// Package ports implements a Memogo-backed port allocator for aigoproxy.
//
// The allocator enforces that no two aigoproxy-managed services
// (i.e. services registered via aigoproxy_register) accidentally try
// to bind the same port. It does NOT arbitrate ports used by
// pre-existing system services or services that pre-date the
// allocator: for those, the operator must check `ss -tlnp` manually.
//
// State layout (namespace "aigoproxy:ports"):
//
//	port:<N>    → JSON {port, owner, allocated_at, note}
//
// Conflict policy:
//   - Claiming a free port succeeds and records the owner.
//   - Claiming a port that belongs to you is a no-op (idempotent).
//   - Claiming a port that belongs to someone else returns
//     {status: "taken", owner: <other>, port: <N>}. The caller
//     should then try the next port in its fallback list.
//   - Releasing a port you own deletes the key.
//   - Releasing a port you don't own is rejected.
package ports

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/biodoia/aigoproxy/internal/store"
)

// MemogoAPI is the minimal interface the allocator needs. The
// store.HTTPMemogoClient satisfies it. Defined here as an interface
// so tests can inject fakes.
//
// We re-use store.MemogoEntry rather than redeclaring the type so
// the two packages stay in lockstep — a V2List response shape
// change in store propagates here automatically.
type MemogoAPI = store.MemogoClient

// MemogoEntry is an alias for store.MemogoEntry, kept here so tests
// in this package don't have to import store just to declare one.
type MemogoEntry = store.MemogoEntry

// Allocator is a port-allocator client. It depends on the same
// Memogo v2 client as the route store but uses a separate namespace
// so the two don't collide.
type Allocator struct {
	api MemogoAPI
	ns  string
}

// New returns a new Allocator using the given Memogo API.
func New(api MemogoAPI) *Allocator {
	return &Allocator{api: api, ns: "aigoproxy:ports"}
}

// Reservation is one port claim.
type Reservation struct {
	Port         int       `json:"port"`
	Owner        string    `json:"owner"`
	AllocatedAt  time.Time `json:"allocated_at"`
	Note         string    `json:"note,omitempty"`
}

// ClaimResult is the response of Claim.
type ClaimResult struct {
	Status string       `json:"status"`          // "ok" | "taken" | "owned"
	Port   int          `json:"port"`
	Owner  string       `json:"owner,omitempty"`  // set when Status="owned" or "taken"
	Note   string       `json:"note,omitempty"`
}

// ListResult is the response of List.
type ListResult struct {
	Reservations []Reservation `json:"reservations"`
	Free         []int         `json:"free"`         // ports that are NOT in the reservation table and look interesting
}

// Claim reserves a port for owner. See package doc for the
// conflict policy.
func (a *Allocator) Claim(ctx context.Context, port int, owner, note string) (ClaimResult, error) {
	existing, err := a.getReservation(ctx, port)
	if err != nil {
		return ClaimResult{}, err
	}
	if existing != nil {
		if existing.Owner == owner {
			// Idempotent: claim succeeds, status "owned"
			return ClaimResult{Status: "owned", Port: port, Owner: owner, Note: existing.Note}, nil
		}
		return ClaimResult{Status: "taken", Port: port, Owner: existing.Owner, Note: existing.Note}, nil
	}
	r := Reservation{
		Port:        port,
		Owner:       owner,
		AllocatedAt: time.Now(),
		Note:        note,
	}
	data, _ := json.Marshal(r)
	if err := a.api.V2Store(ctx, a.ns, fmt.Sprintf("port:%d", port), data, map[string]string{
		"type":  "port-claim",
		"owner": owner,
	}); err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{Status: "ok", Port: port, Owner: owner}, nil
}

// Release releases a port previously claimed by owner. Returns an
// error if the port is owned by someone else.
func (a *Allocator) Release(ctx context.Context, port int, owner string) error {
	existing, err := a.getReservation(ctx, port)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("port %d not reserved", port)
	}
	if existing.Owner != owner {
		return fmt.Errorf("port %d owned by %q, not %q", port, existing.Owner, owner)
	}
	return a.api.V2Delete(ctx, a.ns, fmt.Sprintf("port:%d", port))
}

// List returns all current reservations and a list of "interesting"
// free ports (the well-known dev/service ports not currently taken).
func (a *Allocator) List(ctx context.Context) (ListResult, error) {
	entries, err := a.api.V2List(ctx, a.ns)
	if err != nil {
		return ListResult{}, err
	}
	res := ListResult{Reservations: []Reservation{}}
	taken := map[int]bool{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, "port:") {
			continue
		}
		var r Reservation
		if err := json.Unmarshal([]byte(e.Data), &r); err != nil {
			continue
		}
		// Decode the base64 if Memogo returned it encoded
		// (the underlying client handles that for us, see
		// store.decodeIfBase64).
		res.Reservations = append(res.Reservations, r)
		taken[r.Port] = true
	}
	sort.Slice(res.Reservations, func(i, j int) bool {
		return res.Reservations[i].Port < res.Reservations[j].Port
	})
	// "Interesting" ports: well-known dev/service ports in
	// 3000-10000 range that are NOT currently reserved. Useful
	// for agents to know "what's free without me having to scan".
	for _, p := range defaultInterestingPorts {
		if !taken[p] {
			res.Free = append(res.Free, p)
		}
	}
	return res, nil
}

// IsFree returns true if the port is not currently reserved.
// Note: this does NOT check whether the OS already has the port
// bound by an unrelated process. Use ss -tlnp for that.
func (a *Allocator) IsFree(ctx context.Context, port int) (bool, error) {
	existing, err := a.getReservation(ctx, port)
	if err != nil {
		return false, err
	}
	return existing == nil, nil
}

// NextFree returns the first port in candidates that is not
// currently reserved. Useful for agents that have a fallback list
// and need to pick one without trying each in turn.
func (a *Allocator) NextFree(ctx context.Context, candidates []int) (int, error) {
	for _, p := range candidates {
		free, err := a.IsFree(ctx, p)
		if err != nil {
			return 0, err
		}
		if free {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in candidate list")
}

// getReservation reads a single port's reservation. Returns
// (nil, nil) when the port is not reserved.
func (a *Allocator) getReservation(ctx context.Context, port int) (*Reservation, error) {
	data, _, err := a.api.V2Get(ctx, a.ns, fmt.Sprintf("port:%d", port))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	var r Reservation
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// defaultInterestingPorts is the set of "well-known dev/service
// ports" the allocator checks for the List() result. Used as a
// hint to agents about what's free in the common range.
var defaultInterestingPorts = []int{
	// Standard dev servers
	3000, 3001, 3002, 3003, 3010, 3020, 3030,
	// Common web/SaaS
	4000, 4040, 4200, 4321, 5000, 5050, 5173, 5321, 5500, 5555, 5601, 5800, 5984,
	// Databases (usually not exposed but checked)
	5984, 6379, 7000, 7474, 8000, 8001, 8080, 8081, 8082, 8083, 8084, 8085, 8086, 8087, 8088, 8089, 8090,
	// 8xxx for known dev
	8200, 8500, 8888, 9000, 9001, 9090, 9091, 9200, 9300, 9443,
	// 10-12k ranges (often used for second-instance services)
	10000, 11000, 11001, 11002,
	13000, 13001, 13002, 13080, 13081,
	13100, 13101,
	15678, 15679,
	18000, 18001,
	18080,
	23000, 23001,
	30000, 33000, 33001,
	// LLM/RAG/AI typical
	42110, 42111, 50051, 50060, 50080,
}
