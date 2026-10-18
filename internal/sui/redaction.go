package sui

import "log/slog"

func (r ClientRecord) LogValue() slog.Value { return slog.StringValue(r.String()) }
func (d CreateDraft) LogValue() slog.Value  { return slog.StringValue(d.String()) }
func (d EnableDraft) LogValue() slog.Value  { return slog.StringValue(d.String()) }
func (Binding) String() string              { return "[private SUI binding]" }
func (Binding) GoString() string            { return "[private SUI binding]" }
func (b Binding) LogValue() slog.Value      { return slog.StringValue(b.String()) }
