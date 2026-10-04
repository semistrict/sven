// Decrypts inputs the free sven API stored in R2, printing each as JSON.
//
//   wrangler r2 object get sven-inputs/inputs/2026-10-03/<id> --file box --remote
//   SVEN_ENCRYPTION_KEY=... node scripts/open.mjs box [box...]
//
// A box is a 12-byte AES-GCM IV followed by the ciphertext and tag.
import { readFile } from "node:fs/promises";

const key = await crypto.subtle.importKey(
  "raw",
  Buffer.from(process.env.SVEN_ENCRYPTION_KEY ?? "", "base64"),
  "AES-GCM",
  false,
  ["decrypt"],
);
for (const path of process.argv.slice(2)) {
  const box = await readFile(path);
  const plaintext = await crypto.subtle.decrypt({ name: "AES-GCM", iv: box.subarray(0, 12) }, key, box.subarray(12));
  console.log(new TextDecoder().decode(plaintext));
}
