package configdelivery

import (
	"encoding/json"
	"log/slog"
)

// Stringer alone is insufficient for a structured JSON handler, which may
// otherwise serialize exported credentials and sensitive journal payloads.
func (c Config) LogValue() slog.Value      { return slog.StringValue(c.String()) }
func (r Request) LogValue() slog.Value     { return slog.StringValue(r.String()) }
func (e Entitlement) LogValue() slog.Value { return slog.StringValue(e.String()) }
func (v View) LogValue() slog.Value        { return slog.StringValue(v.String()) }
func (s *Service) LogValue() slog.Value    { return slog.StringValue(s.String()) }

// These application-level wrappers have no wire/storage JSON contract. Hide
// their contents even when nested inside a structured log or diagnostic DTO.
// Fingerprint intentionally marshals an explicit private projection instead.
func (c Config) MarshalJSON() ([]byte, error)      { return json.Marshal(c.String()) }
func (r Request) MarshalJSON() ([]byte, error)     { return json.Marshal(r.String()) }
func (e Entitlement) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }
func (v View) MarshalJSON() ([]byte, error)        { return json.Marshal(v.String()) }
