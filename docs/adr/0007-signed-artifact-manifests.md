# ADR 0007: Signed artifact manifests with DSSE and Ed25519

Status: Accepted

## Context

A device must be able to reject a tampered, corrupted, or substituted update, even if the control plane or the
network between them is compromised. A signature that only covers the file isn't enough: it wouldn't stop a
valid old release being offered as new, or one platform's build being offered to another. Devices also fetch in
chunks across unreliable links, so each chunk has to be checkable by itself.

Checked on 2026-11-05: the DSSE spec (v1.0.2), `sigstore/sigstore-go`, `sigstore/cosign`,
`secure-systems-lab/go-securesystemslib`, and `theupdateframework/go-tuf` are all actively maintained.

## Decision

An artifact is described by a **manifest**: name, version, platform, release counter, total size, full SHA-256,
chunk size, and the SHA-256 of every chunk. The manifest is what gets signed. Since the digests are inside it,
the signature covers every byte of the artifact and every piece of metadata a device relies on.

The manifest is carried in a [DSSE](https://github.com/secure-systems-lab/dsse) envelope, signed with Ed25519
from Go's standard library. DSSE's encoding binds the payload's type into the signature, so a signature made for
some other purpose can't be replayed as a manifest. Its small encoding function is implemented directly, and tested
against the spec's vector.

Trust is a list of public keys given to each device and to the server. A key ID in an envelope is only a hint and
is never used to decide anything, as the spec requires.

**Devices verify independently.** The server also verifies at registration, but that only rejects bad input early.
A device trusts nothing from the server except what the signature covers.

Artifacts are opaque bytes. Nothing unpacks or runs them, which rules out archive extraction attacks. Installation
is simulated, as the project excludes real hardware.

Alternatives considered:

- **Sigstore keyless signing.** It's the strongest option for public software, but needs OIDC, Fulcio, and Rekor at
  signing time and a trust root kept current on the verifier. Devices that are offline for long periods make that
  a poor fit, and `sigstore-go` adds a large dependency tree.
- **Cosign with a key pair.** It would work, but adds a dependency and a format built around container images and
  transparency logs for what's a small amount of standard cryptography.
- **`go-securesystemslib/dsse`.** A reasonable library, with no tagged releases. The part needed here is one function
  defined exactly by the spec, and the signatures come from the standard library.
- **A full TUF implementation.** It's out of scope for v1. It also protects against freeze attacks and key
  compromise, which this design doesn't.
- **A detached signature over the file.** It doesn't bind any metadata, and a device could only check it after
  downloading everything.

## Consequences

- A compromised signing key can sign anything until every device's trust list changes. There's no revocation or
  key rotation protocol, only a list that can hold more than one key during a manual rotation.
- A server that withholds new releases isn't detectable by a device (a freeze attack).
- The release counter in the signed manifest is what makes downgrade protection work, not the version label.
- Signing happens where the private key is, on the operator's machine. The server never holds a private key.
