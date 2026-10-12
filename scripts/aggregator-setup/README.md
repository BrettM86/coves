# Aggregator setup

These scripts give an aggregator an atProto account, a service declaration and a Coves API key. The server-side contract they set up is in [docs/AGGREGATORS.md](../../docs/AGGREGATORS.md).

The minimal path is steps 1, 4 and 5. Steps 2 and 3 are optional: Coves recognizes an aggregator by its indexed service record, and registration gates nothing. Run them only if you want a custom domain handle on record.

Each script reads and appends to `aggregator-config.env` in the current directory. Step 1 writes the account password and JWTs there, so keep the file out of version control.

Use one Coves instance URL for every step and for posting. Step 3 prompts for it (default `https://api.coves.social`) and saves it as `COVES_INSTANCE_URL`. Step 5 reads that variable from the config file and only prompts when the file is missing, so on the minimal path add `COVES_INSTANCE_URL="<url>"` to `aggregator-config.env` before step 5, or its login link has no host. On the local stack under `make run-web` that is `http://127.0.0.1:8080`, never `localhost` (see [docs/LOCAL_DEVELOPMENT.md](../../docs/LOCAL_DEVELOPMENT.md)).

## Steps

```bash
./1-create-pds-account.sh
./2-setup-wellknown.sh            # optional
./3-register-with-coves.sh        # optional, after the .well-known file is live
./4-create-service-declaration.sh
./5-create-api-key.sh
```

### 1. PDS account

`1-create-pds-account.sh` calls `com.atproto.server.createAccount` on the PDS you name and saves the DID, handle, PDS URL, password and session JWTs. Any PDS works. Locally, use `http://localhost:3001` and a `*.local.coves.dev` handle.

### 2. Domain file

`2-setup-wellknown.sh` writes `.well-known/atproto-did` containing your DID, plus example nginx and Apache configs. Upload it so `https://<domain>/.well-known/atproto-did` returns the DID over HTTPS.

### 3. Registration

`3-register-with-coves.sh` checks the file, then calls `social.coves.aggregator.register` with `{did, domain}`. On success Coves writes a `users` row for the DID.

Coves requires the DID's handle to be the domain. A PDS-assigned handle such as `mybot.bsky.social` with domain `rss-bot.example.com` fails with `HandleMismatch`. To register a custom domain, serve the `.well-known` file, switch the account's handle to the domain with `com.atproto.identity.updateHandle`, then run step 3. Other refusals: `DomainVerificationFailed` (file missing or wrong DID), `DIDResolutionFailed`, `AccountErased`, and `AlreadyRegistered`, which is harmless on a re-run.

### 4. Service declaration

`4-create-service-declaration.sh` writes `social.coves.aggregator.service` with rkey `self` to your repo through `com.atproto.repo.createRecord`, using the access JWT from step 1. `displayName` is required. Access JWTs are short-lived: if this step reports an expired token, create a new session with `com.atproto.server.createSession` and update `AGGREGATOR_ACCESS_JWT` in the config file.

The Jetstream consumer indexes the record into `aggregators` within seconds. Check it with:

```bash
curl "$COVES_URL/xrpc/social.coves.aggregator.getServices?dids=$AGGREGATOR_DID"
```

Re-running this script fails because the record already exists. To change the declaration, write it with `com.atproto.repo.putRecord`. Do not delete and re-create it: the delete removes every community authorization, the API key and the stored OAuth session from the AppView, and the authorizations do not come back.

### 5. API key

`5-create-api-key.sh` walks you through minting the key. Its "Checking if aggregator is indexed" probe calls an endpoint that does not exist, so ignore its verdict and use the `getServices` check above.

1. Open `$COVES_URL/oauth/login?handle=<aggregator handle>` in a browser and sign in as the aggregator account.
2. In that page's developer console, run:

   ```js
   fetch('/xrpc/social.coves.aggregator.createApiKey', {method: 'POST', credentials: 'include'})
     .then(r => r.json()).then(d => console.log(d.key))
   ```

3. Paste the key into the script. It must match `ckapi_` plus 64 hex characters.

The key is shown once. Minting another replaces it immediately.

Do not log out of the browser session that minted the key. Logout revokes the OAuth tokens the key runs on, and the key starts failing with 401 within the hour. Close the tab, clear the cookie, or sign in as a different account instead. If it happens, sign in again and mint a new key.

The aggregator's `.env` needs only:

```bash
COVES_API_KEY=ckapi_...
COVES_API_URL=<the instance URL used above>
```

## Authorization

A DID listed in the AppView's `TRUSTED_AGGREGATOR_DIDS` can post to any community. Any other aggregator needs a `social.coves.aggregator.authorization` record in the target community's repo. No Coves endpoint writes that record yet, so it has to be written to the community repo directly with the community's credentials. Without it, posts fail with 403 `NotAuthorized`.

## Posting

```bash
curl -X POST "$COVES_URL/xrpc/social.coves.community.post.create" \
  -H "Authorization: Bearer $COVES_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "community": "<community handle or DID on this instance>",
    "title": "Example headline",
    "content": "Post body",
    "embed": {
      "$type": "social.coves.embed.external",
      "external": {"uri": "https://example.com/story"}
    }
  }'
```

Only `community` is required. The AppView writes the post to the aggregator's own repo.

## Local dev shortcut

`go run scripts/setup_dev_aggregator.go` (from the repo root) creates `test-aggregator.local.coves.dev` on the local PDS and inserts its `users` and `aggregators` rows straight into the dev database, which replaces steps 1 to 4. Then do step 5 against `http://127.0.0.1:8080`. To post without an authorization record, make sure the DID it prints is in `TRUSTED_AGGREGATOR_DIDS` in `.env.dev`; a reset of the local PLC gives the account a new DID.

## Kagi News

Set up the Kagi News account with the numbered scripts. `aggregators/kagi-news/scripts/setup.sh` tries steps 1 to 4 in one pass but cannot finish: it creates a `*.bsky.social` handle, so registration fails with `HandleMismatch`. See [aggregators/kagi-news/README.md](../../aggregators/kagi-news/README.md).
