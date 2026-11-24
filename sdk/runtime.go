package sdk

import "context"

const (
	RuntimeVersion    = 1
	runtimeCapability = "runtime"
	runtimeStopMethod = "ext.runtime.stop"
)

// StopRuntime asks the listener to begin an orderly shutdown. A transport
// failure after the request is indeterminate: do not retry it against a
// possibly replacement listener. Confirm that the original epoch disappeared.
func (c *Client) StopRuntime(ctx context.Context) error {
	s, err := c.connect(ctx)
	if err != nil {
		return err
	}
	if s.hello.Capabilities[runtimeCapability] != RuntimeVersion {
		return &ServerError{Code: "unsupported_capability", Message: "Runtime shutdown was not negotiated", Capability: runtimeCapability}
	}
	_, err = s.call(ctx, runtimeStopMethod, struct{}{}, func(v any) error {
		result, ok := v.(map[string]any)
		if !ok || len(result) != 0 {
			return ErrProtocol
		}
		return nil
	})
	return err
}
