export type LogLevel = "debug" | "info" | "warn" | "error";
type Fields = Record<string, string | number | boolean>;

export type Logger = Record<LogLevel, (message: string, fields?: Fields) => void>;

const severity = { debug: 0, info: 1, warn: 2, error: 3 };

export function createLogger(minimum: LogLevel): Logger {
  let blocked = false;
  function write(level: LogLevel, message: string, fields?: Fields): void {
    if (severity[level] < severity[minimum] || blocked) return;
    blocked = !process.stderr.write(JSON.stringify({
      ...fields, time: new Date().toISOString(), level, message,
    }) + "\n");
    if (blocked) process.stderr.once("drain", () => { blocked = false; });
  }
  return {
    debug: (msg, fields) => write("debug", msg, fields),
    info: (msg, fields) => write("info", msg, fields),
    warn: (msg, fields) => write("warn", msg, fields),
    error: (msg, fields) => write("error", msg, fields),
  };
}
