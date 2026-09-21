# Personal API and MCP keys

Open **Profile → API & MCP**, give the key a name, and choose its permissions. Read only is the default; read/edit and file scopes are available to accounts that can edit. Only owners can mint admin scope. Keys never grant more than the account's current role and proposition membership, and permission changes take effect on the next request. Use a separate key for each client. Copy the key when created; it cannot be recovered later. Revoke it from the same page when finished.

The activity panel shows the account name with **via API** and the key name, or **via MCP** and the reported client name (falling back to the key name for older clients). The authenticated account identity is authoritative; client names are informational. The database keeps its existing `token:<name>` and `mcp:<name>` attribution values.

## Agent instructions

Give your agent the public `/SKILLS.md` URL from Profile, or download the Markdown file. It describes the API/MCP connection, permissions, workflows, retry keys and conflict handling. The guide links to `/api.md` and `/connections.md` from the same deployed app. These documents contain no account data or credentials and can be fetched without authentication. Supply your personal key separately through the client's secret configuration.

## Claude Desktop

1. Install a current Node.js LTS version on your computer.
2. Open Claude Desktop's **Settings → Developer → Edit Config**.
3. Copy the configuration from the profile's **Connect Claude Desktop** panel. Merge its `theses` entry into your existing `mcpServers` object if other servers are configured.
4. Replace `PASTE_YOUR_KEY_HERE` with the personal key, keeping `Bearer ` before it. Keep this configuration private.
5. Save, fully quit Claude Desktop, and reopen it. Enable Theses in the conversation tools/connectors menu. Ask Claude to call `whoami` to verify the account.

The generated configuration uses `mcp-remote@0.1.38`, a third-party local bridge, with Streamable HTTP, `--protocol auto`, and an Authorization header expanded from an environment variable. Automatic protocol translation connects legacy desktop clients to the server's newer stateless MCP protocol. The template never includes a live key. Its endpoint uses the configured workspace base URL. HTTPS is required across the network; localhost HTTP adds `--allow-http` for development.

If `npx` is not found by the desktop app, replace the command with its absolute path. On Windows, use the npm/npx executable installed with Node. The [official MCP desktop guide](https://modelcontextprotocol.io/docs/develop/connect-local-servers) covers configuration locations and restart troubleshooting; the [bridge documentation](https://github.com/punkpeye/mcp-remote) explains header handling and protocol translation.

This local configuration applies to Claude Desktop. It does not configure Claude's web app or Cowork; those use [remote connectors](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp), whose sign-in mechanism differs from this bearer-key setup.

## Other clients and REST

For a client supporting Streamable HTTP and custom headers, use the profile's MCP endpoint and `Authorization: Bearer YOUR_KEY`. For REST, use the profile's API base URL and the same header. `GET /api/v1/me` verifies identity and scopes. Use HTTPS except for localhost development. Never put credentials in URLs.

A 401 means the key is absent, invalid, expired or revoked. A 403 means the key or account lacks a required permission. An inaccessible proposition returns 404. After a backup restore, all keys and calendar subscriptions must be recreated; restored copies of previously revoked credentials are deliberately removed.

Personal keys can expire after 7, 30, 90 or 365 days, or have no expiry. The form defaults to 30 days. Replace an expiring key in your client before its expiry; existing keys are unchanged.
