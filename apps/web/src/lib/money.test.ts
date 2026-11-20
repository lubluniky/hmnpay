import assert from "node:assert/strict"
import { test } from "node:test"

import {
  addAmounts,
  compareAmounts,
  displayAmount,
  displayParts,
  formatUnits,
  isDecimal,
  isZeroAmount,
  amountInputMessage,
  parseAmountInput,
  parseUnits,
  percentChange,
  ratioPercent,
  subtractAmounts,
  sumAmounts,
  toUnits,
} from "./money.ts"

const ONE = BigInt("1000000000000000000")

test("parseUnits reads decimal strings into 18-decimal base units without loss", () => {
  assert.equal(parseUnits("1"), ONE)
  assert.equal(parseUnits("149.00"), BigInt(149) * ONE)
  assert.equal(parseUnits("0.000000000000000001"), BigInt(1))
  assert.equal(parseUnits("0.1"), BigInt("100000000000000000"))
  // The classic float trap: 0.1 + 0.2.
  assert.equal(parseUnits("0.1")! + parseUnits("0.2")!, parseUnits("0.3"))
  // More digits than a float can hold.
  assert.equal(parseUnits("123456789012345678901234567890.123456789012345678"), BigInt("123456789012345678901234567890123456789012345678"))
  assert.equal(parseUnits("7", 0), BigInt(7))
  assert.equal(parseUnits("12.5", 6), BigInt(12500000))
})

test("parseUnits refuses what is not a plain non-negative decimal", () => {
  for (const bad of ["", " ", "-1", "+1", "1e3", "1,5", "1.", ".5", "0x10", "1.2.3", "１２", "NaN", "Infinity", "1 000", "abc"]) {
    assert.equal(parseUnits(bad), null, bad)
    assert.equal(isDecimal(bad), false, bad)
  }
  // Too precise is refused, never rounded; trailing zeros beyond the precision are harmless.
  assert.equal(parseUnits("0.0000000000000000001"), null)
  assert.equal(parseUnits("1.5000000000000000000"), parseUnits("1.5"))
  assert.equal(parseUnits("1.2345678", 6), null)
  assert.throws(() => toUnits("oops"))
})

test("formatUnits prints like the API: trailing zeros trimmed, two decimals kept", () => {
  assert.equal(formatUnits(BigInt(149) * ONE), "149.00")
  assert.equal(formatUnits(ONE / BigInt(2)), "0.50")
  assert.equal(formatUnits(BigInt("12345678000000000000")), "12.345678")
  assert.equal(formatUnits(BigInt(1)), "0.000000000000000001")
  assert.equal(formatUnits(BigInt(0)), "0.00")
  assert.equal(formatUnits(-ONE - ONE / BigInt(4)), "-1.25")
  assert.equal(formatUnits(BigInt(5), 0), "5")
  assert.equal(formatUnits(BigInt(1500000), 6), "1.50")
  // Round trip for a spread of amounts.
  for (const amount of ["0.00", "1.00", "0.01", "999999999.999999999999999999", "25.00", "0.000000000000000001", "100000.10"]) {
    assert.equal(formatUnits(toUnits(amount)), amount)
  }
})

test("arithmetic is exact", () => {
  assert.equal(addAmounts("0.10", "0.20"), "0.30")
  assert.equal(subtractAmounts("25.00", "10.00"), "15.00")
  assert.equal(subtractAmounts("10.00", "25.00"), "-15.00")
  assert.equal(subtractAmounts("1.00", "0.999999999999999999"), "0.000000000000000001")
  assert.equal(sumAmounts(["0.10", "0.20", "0.30", "149.00"]), "149.60")
  assert.equal(sumAmounts([]), "0.00")
  // A sum a float gets wrong (0.1 * 3 = 0.30000000000000004).
  assert.equal(sumAmounts(["0.1", "0.1", "0.1"]), "0.30")
  assert.equal(compareAmounts("1.00", "1"), 0)
  assert.equal(compareAmounts("0.999999999999999999", "1"), -1)
  assert.equal(compareAmounts("1.000000000000000001", "1"), 1)
  assert.equal(isZeroAmount("0.00"), true)
  assert.equal(isZeroAmount("0.000000000000000001"), false)
})

test("display keeps every digit and groups thousands", () => {
  assert.deepEqual(displayParts("1234567.5"), { negative: false, whole: "1,234,567", fraction: "50" })
  assert.equal(displayAmount("149.00"), "149.00")
  assert.equal(displayAmount("1000"), "1,000.00")
  assert.equal(displayAmount("0.000000000000000001"), "0.000000000000000001")
  assert.equal(displayAmount("12.345678"), "12.345678")
  assert.equal(displayAmount("-21.03"), "-21.03")
  assert.equal(displayAmount("007.5"), "7.50")
  assert.equal(displayAmount("5", 0), "5")
  assert.equal(displayAmount("999999999999999999999.99"), "999,999,999,999,999,999,999.99")
})

test("an amount field accepts digits with at most one dot, and guesses nothing", () => {
  const ok = (input: string, amount: string) => assert.deepEqual(parseAmountInput(input), { ok: true, amount }, input)
  ok("25", "25")
  ok("25.00", "25")
  ok("49.90", "49.9")
  ok("1250", "1250")
  ok("1250.50", "1250.5")
  ok(" 1250.50 ", "1250.5") // space around the amount is not part of it
  ok("0007", "7")
  ok("0.5", "0.5")
  ok("0.000000000000000001", "0.000000000000000001")
  const bad = (input: string, reason: string) => assert.deepEqual(parseAmountInput(input), { ok: false, reason }, input)
  bad("", "empty")
  bad("   ", "empty")
  bad("0", "zero")
  bad("0.00", "zero")
  // A comma is never read as anything: not as a decimal point, not as a thousands separator.
  bad("1,250", "separator")
  bad("1,250.50", "separator")
  bad("12,5", "separator")
  bad("1.250,50", "separator")
  bad("1 250.50", "separator")
  bad("1\u00a0250", "separator")
  bad("1'250", "separator")
  bad("1_250", "separator")
  bad("1.2.3", "separator")
  bad(".5", "separator")
  bad("5.", "separator")
  bad("-5", "invalid")
  bad("+5", "invalid")
  bad("1e5", "invalid")
  bad("0x10", "invalid")
  bad("abc", "invalid")
  bad("12 USDT", "invalid")
  bad("１２", "invalid") // full-width digits are not digits here
  bad("0.0000000000000000001", "too_precise")
  assert.deepEqual(parseAmountInput("1.2345678", 6), { ok: false, reason: "too_precise" })
  assert.deepEqual(parseAmountInput("1.234567", 6), { ok: true, amount: "1.234567" })
  // Whatever is accepted is what was typed, digit for digit: no input turns into another amount.
  for (const input of ["1,250", "1.250", "1250", "1 250", "1,25", "1.25", "125,0", "12.50", "12,50"]) {
    const parsed = parseAmountInput(input)
    if (parsed.ok) assert.equal(parseUnits(parsed.amount), parseUnits(input.trim()), input)
  }
  for (const reason of ["empty", "separator", "invalid", "too_precise", "zero"] as const) assert.ok(amountInputMessage(reason).length > 10)
})

test("ratios and changes are computed on base units", () => {
  assert.equal(ratioPercent("10.00", "25.00"), 40)
  assert.equal(ratioPercent("0", "25.00"), 0)
  assert.equal(ratioPercent("30", "25"), 100) // capped: a bar is never over-full
  assert.equal(ratioPercent("1", "0"), 0)
  assert.equal(ratioPercent("1", "3"), 33.33)
  assert.equal(percentChange("110.00", "100.00"), 10)
  assert.equal(percentChange("90.00", "100.00"), -10)
  assert.equal(percentChange("100.00", "0.00"), null)
  assert.equal(percentChange("0.30", "0.10"), 200)
})
