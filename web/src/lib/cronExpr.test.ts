import { describe, it, expect } from "vitest";
import {
  CRON_MAX_LINES,
  detectInterval,
  humanizeCron,
  isCronExprSyntaxValid,
  normalizeCronExpr,
} from "./cronExpr";
import { setLocale } from "./i18n";

const OPTS = { minutes: [5, 10, 15, 30], hours: [1, 2, 3], days: [1, 2] };

describe("multi-line cron expressions", () => {
  it("accepts multiple valid lines", () => {
    expect(isCronExprSyntaxValid("*/30 * * * 1-5\n0 */3 * * 0,6")).toBe(true);
    expect(isCronExprSyntaxValid("*/30 * * * 1-5\r\n0 */3 * * 0,6")).toBe(true);
  });

  it("rejects a bad line, a mixed-in sentinel, and too many lines", () => {
    expect(isCronExprSyntaxValid("*/30 * * * 1-5\n0 */3 * *")).toBe(false);
    expect(isCronExprSyntaxValid("*/30 * * * 1-5\n*/0 * * * *")).toBe(false);
    expect(isCronExprSyntaxValid("*/30 * * * 1-5\n@preset:30")).toBe(false);
    const many = Array(CRON_MAX_LINES + 1).fill("0 9 * * *").join("\n");
    expect(isCronExprSyntaxValid(many)).toBe(false);
    const long = Array(CRON_MAX_LINES).fill(`${"0,".repeat(40)}0 9 * * *`).join("\n");
    expect(isCronExprSyntaxValid(long)).toBe(false);
  });

  it("normalises blank lines and padding away", () => {
    expect(normalizeCronExpr("  */30 * * * 1-5 \n\n 0 */3 * * 0,6\n")).toBe(
      "*/30 * * * 1-5\n0 */3 * * 0,6",
    );
    expect(normalizeCronExpr("\n \n")).toBe("");
  });

  it("is never treated as an interval", () => {
    expect(detectInterval("*/30 * * * *\n*/30 * * * *", OPTS)).toBeNull();
  });

  it("humanizes each line", () => {
    setLocale("en");
    expect(humanizeCron("0 9 * * *\n30 18 * * *")).toBe("daily 09:00 / daily 18:30");
  });
});
