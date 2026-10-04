// Stored inputs are sealed with AES-256-GCM: a random 12-byte IV followed by
// the ciphertext and its tag.

const ivBytes = 12;

async function importKey(base64Key: string, usage: "encrypt" | "decrypt"): Promise<CryptoKey> {
  const raw = Uint8Array.from(atob(base64Key), (c) => c.charCodeAt(0));
  if (raw.length !== 32) {
    throw new Error(`encryption key is ${raw.length} bytes, want 32`);
  }
  return crypto.subtle.importKey("raw", raw, "AES-GCM", false, [usage]);
}

export async function seal(base64Key: string, plaintext: Uint8Array): Promise<Uint8Array> {
  const key = await importKey(base64Key, "encrypt");
  const iv = crypto.getRandomValues(new Uint8Array(ivBytes));
  const sealed = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, plaintext));
  const out = new Uint8Array(ivBytes + sealed.length);
  out.set(iv);
  out.set(sealed, ivBytes);
  return out;
}

export async function open(base64Key: string, box: Uint8Array): Promise<Uint8Array> {
  const key = await importKey(base64Key, "decrypt");
  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: box.slice(0, ivBytes) },
    key,
    box.slice(ivBytes),
  );
  return new Uint8Array(plaintext);
}
