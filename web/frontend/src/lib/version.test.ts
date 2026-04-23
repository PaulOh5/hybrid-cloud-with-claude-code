import { describe, it, expect } from "vitest";
import { appVersion } from "./version";

describe("appVersion", () => {
  it("follows semver", () => {
    expect(appVersion).toMatch(/^\d+\.\d+\.\d+$/);
  });
});
