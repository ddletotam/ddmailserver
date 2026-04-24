<script lang="ts">
  import { accountStore } from "../stores/accounts.svelte";

  interface Props {
    onSuccess: () => void;
  }
  let { onSuccess }: Props = $props();

  let name = $state("");
  let email = $state("");
  let imapHost = $state("mail.letotam.ru");
  let imapPort = $state(993);
  let smtpHost = $state("mail.letotam.ru");
  let smtpPort = $state(465);
  let username = $state("");
  let password = $state("");
  let useTls = $state(true);
  let error = $state("");
  let testing = $state(false);

  async function handleSubmit() {
    if (!name || !email || !username || !password) {
      error = "Fill in all required fields";
      return;
    }

    testing = true;
    error = "";

    try {
      // Try connecting via IMAP to validate credentials
      const { invoke } = await import("@tauri-apps/api/core");
      await invoke("connect", {
        host: imapHost,
        port: imapPort,
        username,
        password,
        useTls,
      });

      accountStore.add({
        name,
        email,
        imap_host: imapHost,
        imap_port: imapPort,
        smtp_host: smtpHost,
        smtp_port: smtpPort,
        username,
        password,
        use_tls: useTls,
      });

      onSuccess();
    } catch (e) {
      error = String(e);
    } finally {
      testing = false;
    }
  }
</script>

<div class="login-container">
  <div class="login-card">
    <div class="login-header">
      <div class="logo">
        <svg width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="var(--text-accent)" stroke-width="1.5">
          <path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z"/>
          <polyline points="22,6 12,13 2,6"/>
        </svg>
      </div>
      <h1>DDMail</h1>
      <p>Add your email account</p>
    </div>

    <form onsubmit={(e) => { e.preventDefault(); handleSubmit(); }}>
      <div class="field">
        <label for="name">Display name</label>
        <input id="name" type="text" bind:value={name} placeholder="John Doe" />
      </div>

      <div class="field">
        <label for="email">Email</label>
        <input id="email" type="email" bind:value={email} placeholder="user@example.com" />
      </div>

      <div class="field">
        <label for="username">Username</label>
        <input id="username" type="text" bind:value={username} placeholder="username" />
      </div>

      <div class="field">
        <label for="password">Password</label>
        <input id="password" type="password" bind:value={password} />
      </div>

      <details class="server-details">
        <summary>Server settings</summary>
        <div class="server-grid">
          <div class="field">
            <label for="imap-host">IMAP host</label>
            <input id="imap-host" type="text" bind:value={imapHost} />
          </div>
          <div class="field">
            <label for="imap-port">Port</label>
            <input id="imap-port" type="number" bind:value={imapPort} />
          </div>
          <div class="field">
            <label for="smtp-host">SMTP host</label>
            <input id="smtp-host" type="text" bind:value={smtpHost} />
          </div>
          <div class="field">
            <label for="smtp-port">Port</label>
            <input id="smtp-port" type="number" bind:value={smtpPort} />
          </div>
        </div>
        <label class="checkbox">
          <input type="checkbox" bind:checked={useTls} />
          Use TLS
        </label>
      </details>

      {#if error}
        <div class="error">{error}</div>
      {/if}

      <button type="submit" class="btn-primary" disabled={testing}>
        {testing ? "Connecting..." : "Sign In"}
      </button>
    </form>
  </div>
</div>

<style>
  .login-container {
    display: flex;
    align-items: center;
    justify-content: center;
    height: 100vh;
    background: var(--bg-secondary);
  }

  .login-card {
    background: var(--bg-primary);
    border-radius: 12px;
    padding: 40px;
    width: 400px;
    box-shadow: var(--shadow-md);
  }

  .login-header {
    text-align: center;
    margin-bottom: 32px;
  }

  .logo {
    margin-bottom: 8px;
    display: flex;
    justify-content: center;
  }

  .login-header h1 {
    font-size: 24px;
    font-weight: 600;
    margin-bottom: 4px;
  }

  .login-header p {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .field {
    margin-bottom: 16px;
  }

  .field label {
    display: block;
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    margin-bottom: 4px;
  }

  .field input {
    width: 100%;
    padding: 10px 12px;
    border: 1px solid var(--border-color);
    border-radius: 8px;
    font-size: var(--font-size);
    font-family: var(--font-family);
    outline: none;
    transition: border-color var(--transition);
  }

  .field input:focus {
    border-color: var(--text-accent);
  }

  .server-details {
    margin-bottom: 16px;
  }

  .server-details summary {
    cursor: pointer;
    color: var(--text-accent);
    font-size: var(--font-size-sm);
    margin-bottom: 12px;
  }

  .server-grid {
    display: grid;
    grid-template-columns: 1fr 100px;
    gap: 8px;
  }

  .checkbox {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: var(--font-size-sm);
    margin-top: 8px;
    cursor: pointer;
  }

  .error {
    background: #fff0f0;
    color: #d32f2f;
    padding: 8px 12px;
    border-radius: 8px;
    font-size: var(--font-size-sm);
    margin-bottom: 16px;
  }

  .btn-primary {
    width: 100%;
    padding: 12px;
    background: var(--bg-active);
    color: white;
    border: none;
    border-radius: 8px;
    font-size: var(--font-size);
    font-weight: 500;
    cursor: pointer;
    transition: opacity var(--transition);
  }

  .btn-primary:hover {
    opacity: 0.9;
  }

  .btn-primary:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
</style>
