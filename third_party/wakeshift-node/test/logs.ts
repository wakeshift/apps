// A logger for tests: every line the SDK logs, with its level, message and fields.
import type { LogFields, Logger } from "../src/index.js";

export interface Line {
  level: "error" | "warn" | "info" | "debug";
  message: string;
  fields: LogFields;
}

/** A logger that keeps every line; lines(level) gives those at that level and above. */
export function captureLog(): { logger: Logger; lines: Line[]; codes(min?: Line["level"]): string[] } {
  const lines: Line[] = [];
  const at = (level: Line["level"]) => (message: string, fields: LogFields) => void lines.push({ level, message, fields });
  const rank = { debug: 0, info: 1, warn: 2, error: 3 };
  return {
    logger: { error: at("error"), warn: at("warn"), info: at("info"), debug: at("debug") },
    lines,
    codes: (min = "info") => lines.filter((l) => rank[l.level] >= rank[min]).map((l) => l.fields.code),
  };
}

/** A logger that pushes each warning's and error's message onto into, for tests that read them. */
export function warningsInto(into: string[]): Logger {
  return { error: (m) => void into.push(m), warn: (m) => void into.push(m), info: () => {}, debug: () => {} };
}

/** A logger that drops everything. */
export const quiet: Logger = { error: () => {}, warn: () => {}, info: () => {}, debug: () => {} };
