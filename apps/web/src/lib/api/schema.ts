/**
 * A very small runtime schema library for the API's wire shapes.
 *
 * Every response type in `types.ts` is declared once, as a schema; the TypeScript type is inferred
 * from it, so the two cannot disagree. `check` compares a decoded JSON value with a schema
 * strictly: a missing key, a key the schema does not know, a wrong type or an unknown enum value
 * is an issue. The end-to-end tests run every response of the real server through it
 * (`validation` in client.ts), which is what fails when the Go DTOs and these types drift apart.
 */

export interface Schema<T> {
  /** Appends a description of everything wrong with `value` to `issues`. */
  check(value: unknown, path: string, issues: string[]): void
  /** Type carrier only; never read. */
  readonly _type?: T
}

export interface OptionalSchema<T> extends Schema<T> {
  readonly optional: true
}

export type Infer<S> = S extends Schema<infer T> ? T : never

const kind = (value: unknown) => (value === null ? "null" : Array.isArray(value) ? "array" : typeof value)

function primitive<T>(name: string, test: (value: unknown) => boolean): Schema<T> {
  return {
    check(value, path, issues) {
      if (!test(value)) issues.push(`${path}: expected ${name}, got ${kind(value)}`)
    },
  }
}

export const str = primitive<string>("string", (v) => typeof v === "string")
export const num = primitive<number>("number", (v) => typeof v === "number" && Number.isFinite(v))
export const int = primitive<number>("integer", (v) => typeof v === "number" && Number.isInteger(v))
export const bool = primitive<boolean>("boolean", (v) => typeof v === "boolean")
/** Anything: for payloads whose shape depends on something else (an event's `data`). */
export const json: Schema<unknown> = { check() {} }

/** RFC 3339 timestamp, as Go's time.Time marshals. */
export const time: Schema<string> = {
  check(value, path, issues) {
    if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/.test(value) || Number.isNaN(Date.parse(value))) {
      issues.push(`${path}: expected an RFC 3339 timestamp, got ${typeof value === "string" ? JSON.stringify(value) : kind(value)}`)
    }
  },
}

/** A decimal amount in whole units of the asset, as core.Amount.Format writes it ("149.00"). */
export const amount: Schema<string> = {
  check(value, path, issues) {
    if (typeof value !== "string" || !/^\d+(\.\d+)?$/.test(value)) {
      issues.push(`${path}: expected a decimal amount string, got ${typeof value === "string" ? JSON.stringify(value) : kind(value)}`)
    }
  },
}

export function oneOf<const T extends readonly string[]>(...values: T): Schema<T[number]> {
  return {
    check(value, path, issues) {
      if (typeof value !== "string" || !values.includes(value)) {
        issues.push(`${path}: expected one of ${values.map((v) => JSON.stringify(v)).join(", ")}, got ${JSON.stringify(value)}`)
      }
    },
  }
}

export function nullable<T>(inner: Schema<T>): Schema<T | null> {
  return {
    check(value, path, issues) {
      if (value !== null) inner.check(value, path, issues)
    },
  }
}

/** A key the server leaves out when it has nothing to say (Go's `omitempty`). */
export function optional<T>(inner: Schema<T>): OptionalSchema<T> {
  return { check: inner.check, optional: true }
}

export function arr<T>(inner: Schema<T>): Schema<T[]> {
  return {
    check(value, path, issues) {
      if (!Array.isArray(value)) {
        issues.push(`${path}: expected array, got ${kind(value)}`)
        return
      }
      value.forEach((item, i) => inner.check(item, `${path}[${i}]`, issues))
    },
  }
}

/** A JSON object used as a map (payment metadata). */
export function record<T>(inner: Schema<T>): Schema<Record<string, T>> {
  return {
    check(value, path, issues) {
      if (kind(value) !== "object") {
        issues.push(`${path}: expected object, got ${kind(value)}`)
        return
      }
      for (const [key, item] of Object.entries(value as object)) inner.check(item, `${path}.${key}`, issues)
    },
  }
}

type Shape = Record<string, Schema<unknown>>
type OptionalKeys<S extends Shape> = { [K in keyof S]: S[K] extends OptionalSchema<unknown> ? K : never }[keyof S]
type Simplify<T> = { [K in keyof T]: T[K] } & {}
export type ObjectOf<S extends Shape> = Simplify<
  { [K in Exclude<keyof S, OptionalKeys<S>>]: Infer<S[K]> } & { [K in OptionalKeys<S>]?: Infer<S[K]> }
>

export function obj<S extends Shape>(shape: S): Schema<ObjectOf<S>> & { shape: S } {
  return {
    shape,
    check(value, path, issues) {
      if (kind(value) !== "object") {
        issues.push(`${path}: expected object, got ${kind(value)}`)
        return
      }
      const record = value as Record<string, unknown>
      for (const [key, schema] of Object.entries(shape)) {
        if (!(key in record)) {
          if (!("optional" in schema)) issues.push(`${path}.${key}: missing`)
          continue
        }
        schema.check(record[key], `${path}.${key}`, issues)
      }
      for (const key of Object.keys(record)) {
        if (!(key in shape)) issues.push(`${path}.${key}: not in the client's type (the API added or renamed a field)`)
      }
    },
  }
}

/** Everything wrong with `value` as a `schema`; empty when it matches exactly. */
export function check<T>(schema: Schema<T>, value: unknown, label = "$"): string[] {
  const issues: string[] = []
  schema.check(value, label, issues)
  return issues
}
