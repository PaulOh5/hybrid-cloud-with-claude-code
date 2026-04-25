import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Vitest doesn't auto-run @testing-library/react's cleanup the way Jest does;
// without this each render leaks DOM into the next test in the file.
afterEach(() => {
  cleanup();
});
