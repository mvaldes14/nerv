package decision

import (
	"context"
	"fmt"

	"github.com/mvaldes14/nerv/internal/investigation"
)

const localProvider = "nerv.local"

// Router keeps operator intent out of the alert-disposition contract and only
// sends supported alert sources to Decision Gateway.
type Router struct {
	remote investigation.DecisionEngine
}

func NewRouter(remote investigation.DecisionEngine) *Router {
	return &Router{remote: remote}
}

func (r *Router) Evaluate(ctx context.Context, req investigation.Request, evidence []investigation.Evidence) (investigation.Decision, error) {
	switch req.Source {
	case investigation.SourceHTTPAsk, investigation.SourceCLI:
		return investigation.Decision{
			Action:   "investigate",
			Reason:   "explicit operator investigation request",
			Provider: localProvider,
		}, nil
	case investigation.SourceHTTPAlert, investigation.SourceKubernetesDetector:
		if r.remote == nil {
			return investigation.Decision{
				Action:   "investigate",
				Reason:   "Decision Gateway not configured; default NERV policy",
				Provider: localProvider,
			}, nil
		}
		return r.remote.Evaluate(ctx, req, evidence)
	default:
		return investigation.Decision{}, fmt.Errorf(
			"unsupported investigation source %q; supported sources are %q, %q, %q, and %q",
			req.Source,
			investigation.SourceHTTPAsk,
			investigation.SourceCLI,
			investigation.SourceHTTPAlert,
			investigation.SourceKubernetesDetector,
		)
	}
}
