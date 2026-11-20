import type { ApiError } from "./client"

/** What a query hook holds: the key it is showing and what it has for it. */
export interface State<T> {
  key: string | null
  data: T | undefined
  error: ApiError | undefined
  loading: boolean
}

/**
 * A mutation made for `key`, applied to the hook's state as it is now. The data is written to
 * the cache under `key` in any case; the state changes only if it is still showing `key`.
 */
export function applyMutation<T>(
  store: Map<string, unknown>,
  key: string,
  state: State<T>,
  next: T | ((current: T | undefined) => T | undefined),
): State<T> {
  const showing = state.key === key
  const base = showing ? state.data : (store.get(key) as T | undefined)
  const data = typeof next === "function" ? (next as (c: T | undefined) => T | undefined)(base) : next
  if (data !== undefined) store.set(key, data)
  return showing ? { ...state, data } : state
}
