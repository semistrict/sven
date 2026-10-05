import { bindings, defineConfig } from "cf/config";

// Secrets, set with `cf workers secrets put` (not stored here):
//   TYPESAFE_API_KEY     the key requests to Jev are made with
//   SVEN_ENCRYPTION_KEY  base64 of 32 random bytes: openssl rand -base64 32
export default defineConfig({
	worker: {
		name: "sven",
		compatibilityDate: "2026-10-01",
		entrypoint: "src/index.ts",
		// sven.ramon3525.workers.dev stays up for clients that predate the domain.
		domains: ["sven.semistrict.com"],
		workersDev: true,
		observability: {
			enabled: true,
		},
		env: {
			// Every request is stored here, sealed with SVEN_ENCRYPTION_KEY.
			INPUTS: bindings.r2({
				name: "sven-inputs",
			}),
			RATE_LIMITER: bindings.rateLimit({
				namespace: "1001",
				simple: {
					limit: 120,
					period: 60,
				},
			}),
		},
	},
});
