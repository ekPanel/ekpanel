// Passkeys (WebAuthn): the panel sends options with base64url strings; the browser wants
// ArrayBuffers, and its answer goes back as base64url JSON.

/* eslint-disable @typescript-eslint/no-explicit-any */

const fromB64u = (s: string): ArrayBuffer => {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4);
  return Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)).buffer;
};

const toB64u = (b: ArrayBuffer | null | undefined): string | undefined => {
  if (!b) return undefined;
  let s = "";
  for (const x of new Uint8Array(b)) s += String.fromCharCode(x);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
};

/** Whether this browser can use passkeys at all (they also need HTTPS, or localhost). */
export const passkeysSupported = () => typeof window !== "undefined" && "PublicKeyCredential" in window && window.isSecureContext;

/** Makes a passkey with the panel's options; returns the answer to send back. */
export async function createPasskey(options: any): Promise<unknown> {
  const pk = options.publicKey;
  const publicKey: PublicKeyCredentialCreationOptions = {
    ...pk,
    challenge: fromB64u(pk.challenge),
    user: { ...pk.user, id: fromB64u(pk.user.id) },
    excludeCredentials: (pk.excludeCredentials ?? []).map((c: any) => ({ ...c, id: fromB64u(c.id) })),
  };
  const cred = (await navigator.credentials.create({ publicKey })) as PublicKeyCredential;
  const r = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: toB64u(cred.rawId),
    type: cred.type,
    response: { clientDataJSON: toB64u(r.clientDataJSON), attestationObject: toB64u(r.attestationObject), transports: r.getTransports?.() ?? [] },
    clientExtensionResults: cred.getClientExtensionResults(),
  };
}

/** Signs in with a passkey the browser offers; returns the answer to send back. */
export async function getPasskey(options: any): Promise<unknown> {
  const pk = options.publicKey;
  const publicKey: PublicKeyCredentialRequestOptions = {
    ...pk,
    challenge: fromB64u(pk.challenge),
    allowCredentials: (pk.allowCredentials ?? []).map((c: any) => ({ ...c, id: fromB64u(c.id) })),
  };
  const cred = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential;
  const r = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: toB64u(cred.rawId),
    type: cred.type,
    response: {
      clientDataJSON: toB64u(r.clientDataJSON),
      authenticatorData: toB64u(r.authenticatorData),
      signature: toB64u(r.signature),
      userHandle: toB64u(r.userHandle),
    },
    clientExtensionResults: cred.getClientExtensionResults(),
  };
}

/** The browser's own cancel (the person closed its dialog) is not an error to show. */
export const cancelled = (err: unknown) => err instanceof DOMException && (err.name === "NotAllowedError" || err.name === "AbortError");
