import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// Tests run the handler directly with their own env, so the runtime only
// needs the R2 bucket that stored inputs land in.
export default defineConfig({
  plugins: [
    cloudflareTest({
      main: "./src/index.ts",
      miniflare: { compatibilityDate: "2026-10-01", r2Buckets: ["INPUTS"] },
    }),
  ],
});
