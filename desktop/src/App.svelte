<script lang="ts">
  import Sidebar from "./lib/components/Sidebar.svelte";
  import ChatView from "./lib/components/ChatView.svelte";
  import LoginScreen from "./lib/components/LoginScreen.svelte";
  import { accountStore } from "./lib/stores/accounts.svelte";
  import { mailStore } from "./lib/stores/mail.svelte";
  import { identityStore } from "./lib/stores/identity.svelte";

  let showLogin = $state(accountStore.accounts.length === 0);

  $effect(() => {
    const account = accountStore.activeAccount;
    if (!account) return;
    // Identities must be available BEFORE the conversation grouping runs — the
    // server-side `fetch_conversations` derives "our addresses" from the cached
    // identity list to compute (counterpart, my_identity) pairs. If we don't wait,
    // a fresh login groups everything under the single account.email and aliases
    // appear as self-threads.
    (async () => {
      await identityStore.load(account);
      await mailStore.loadConversations(account);
    })();
  });

  function handleAccountAdded() {
    showLogin = false;
  }
</script>

{#if showLogin}
  <LoginScreen onSuccess={handleAccountAdded} />
{:else}
  <div class="app-layout">
    <Sidebar />
    <ChatView />
  </div>
{/if}

<style>
  .app-layout {
    display: flex;
    height: 100vh;
    width: 100vw;
    overflow: hidden;
  }
</style>
