// zod 3 and zod 4 side by side, without either MCP SDK: the types and
// helpers the SDK used to take from @modelcontextprotocol/sdk's
// zod-compat, so it loads whichever MCP SDK an app has (host.ts). The
// definitions follow that module's, so a schema is read, checked and
// rendered as JSON Schema the same way as before.

import type * as z3 from "zod/v3";
import type * as z4 from "zod/v4/core";
import * as z3rt from "zod/v3";
import * as z4mini from "zod/v4-mini";
import { zodToJsonSchema } from "zod-to-json-schema";
import { UsageError } from "./errors.js";

/**
 * A zod 3 schema, read by its shape rather than by zod 3's own types: an
 * app's "zod" and the "zod/v3" this package names can resolve to two
 * copies of zod 3's declarations (moduleResolution node10), which compare
 * structurally and too deeply for the compiler (TS2589).
 */
export interface Zod3Schema {
  readonly _def: unknown;
  readonly _output: unknown;
  readonly _input: unknown;
  safeParse(data: unknown): unknown;
}
/** A zod schema, from zod 3's API or zod 4's (classic or mini). */
export type AnySchema = Zod3Schema | z4.$ZodType;
/** An object schema, or any schema (as the MCP SDK takes it). */
export type AnyObjectSchema = z4.$ZodObject | AnySchema;
/** A zod shape: { field: schema }. */
export type ZodRawShapeCompat = Record<string, AnySchema>;
/** What a schema parses to. */
export type SchemaOutput<S> = S extends z4.$ZodType ? z4.output<S> : S extends Zod3Schema ? S["_output"] : never;
/** What a schema takes. */
export type SchemaInput<S> = S extends z4.$ZodType ? z4.input<S> : S extends Zod3Schema ? S["_input"] : never;
/** What a shape parses to, field by field. */
export type ShapeOutput<Shape extends ZodRawShapeCompat> = { [K in keyof Shape]: SchemaOutput<Shape[K]> };

type ParseResult<T> = { success: true; data: T } | { success: false; error: unknown };

/** @internal Whether a schema is zod 4's (classic or mini). */
export function isZ4Schema(s: unknown): s is z4.$ZodType {
  return !!(s as { _zod?: unknown } | null)?._zod;
}

/** @internal Whether a value is a zod schema of either major. */
export function isSchema(v: unknown): v is AnySchema {
  return !!v && typeof v === "object" && ("_def" in (v as object) || "_zod" in (v as object));
}

/** @internal An object schema of a shape, in the zod API its fields use (zod 4 mini for an empty shape). */
export function objectFromShape(shape: ZodRawShapeCompat): AnyObjectSchema {
  const values = Object.values(shape);
  if (values.length === 0) return z4mini.object({});
  if (values.every(isZ4Schema)) return z4mini.object(shape as Record<string, z4.$ZodType>);
  if (values.every((s) => !isZ4Schema(s))) return z3rt.object(shape as Record<string, z3.ZodTypeAny>);
  throw new UsageError("config", "a shape mixes zod 3 and zod 4 schemas");
}

/** @internal Parses with either major. */
export function safeParse<S extends AnySchema>(schema: S, data: unknown): ParseResult<SchemaOutput<S>> {
  if (isZ4Schema(schema)) return z4mini.safeParse(schema, data) as ParseResult<SchemaOutput<S>>;
  return (schema as z3.ZodTypeAny).safeParse(data) as ParseResult<SchemaOutput<S>>;
}

/** @internal Parses with either major, allowing async refinements. */
export async function safeParseAsync<S extends AnySchema>(schema: S, data: unknown): Promise<ParseResult<SchemaOutput<S>>> {
  if (isZ4Schema(schema)) return (await z4mini.safeParseAsync(schema, data)) as ParseResult<SchemaOutput<S>>;
  return (await (schema as z3.ZodTypeAny).safeParseAsync(data)) as ParseResult<SchemaOutput<S>>;
}

/** @internal An object schema's shape (undefined for anything else). */
export function getObjectShape(schema: unknown): Record<string, AnySchema> | undefined {
  if (!schema) return undefined;
  const raw = isZ4Schema(schema)
    ? (schema as { _zod?: { def?: { shape?: unknown } } })._zod?.def?.shape
    : (schema as { shape?: unknown }).shape;
  if (!raw) return undefined;
  if (typeof raw === "function") {
    try {
      return raw() as Record<string, AnySchema>;
    } catch {
      return undefined;
    }
  }
  return raw as Record<string, AnySchema>;
}

/** @internal A schema or shape as an object schema (a shape is wrapped); undefined if it's neither. */
export function normalizeObjectSchema(schema: unknown): AnyObjectSchema | undefined {
  if (!schema || typeof schema !== "object") return undefined;
  const s = schema as { _def?: unknown; _zod?: { def?: { type?: string; shape?: unknown } }; shape?: unknown };
  if (!s._def && !s._zod) {
    const values = Object.values(schema);
    const looksLikeShape =
      values.length > 0 &&
      values.every((v) => typeof v === "object" && v !== null && ((v as { _def?: unknown })._def !== undefined || (v as { _zod?: unknown })._zod !== undefined || typeof (v as { parse?: unknown }).parse === "function"));
    if (looksLikeShape) return objectFromShape(schema as ZodRawShapeCompat);
  }
  if (isZ4Schema(schema)) {
    const def = s._zod?.def;
    if (def && (def.type === "object" || def.shape !== undefined)) return schema as AnyObjectSchema;
  } else if (s.shape !== undefined) {
    return schema as AnyObjectSchema;
  }
  return undefined;
}

// zod 3 schemas are rendered by zod-to-json-schema, with the options
// @modelcontextprotocol/sdk 1.x gives it (zod-json-schema-compat), so
// tools/list reads as it did; zod 4 renders its own. It's a dependency,
// imported statically, so defineEvent never needs an MCP SDK at run time,
// in a bundle or with no MCP SDK installed (a disabled client included).

/** @internal A schema as JSON Schema, the way the MCP SDK 1.x renders one for tools/list (draft-7, input side). */
export function toJsonSchema(schema: AnySchema): Record<string, unknown> {
  if (isZ4Schema(schema)) return z4mini.toJSONSchema(schema, { target: "draft-7", io: "input" }) as Record<string, unknown>;
  return zodToJsonSchema(schema as z3.ZodTypeAny, { strictUnions: true, pipeStrategy: "input" }) as Record<string, unknown>;
}
