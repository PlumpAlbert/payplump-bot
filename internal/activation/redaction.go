package activation

import "log/slog"

func (i Intent) LogValue() slog.Value   { return slog.StringValue(i.String()) }
func (s Snapshot) LogValue() slog.Value { return slog.StringValue(s.String()) }
