package runtime

import "log/slog"

// log is the package-level logger with runtime context attached.
// All runtime package logging should use this logger so that all logs
// automatically carry the "package": "runtime" field.
var log = slog.With("package", "runtime")
