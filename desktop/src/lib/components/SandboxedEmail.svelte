<script lang="ts">
  import { invoke } from "@tauri-apps/api/core";
  import { prepareEmailHtml, type ContentPermissions } from "../utils/html";
  import { accountStore } from "../stores/accounts.svelte";

  interface Props {
    html: string;
    isDark: boolean;
    permissions: ContentPermissions;
    /** DB id of the message — required for resolving `cid:` inline images via
     *  the v2_fetch_inline_part command. When omitted, cid: refs stay broken. */
    messageUid?: number;
  }
  let { html, isDark, permissions, messageUid }: Props = $props();

  let hostRef = $state<HTMLDivElement | null>(null);
  let shadow: ShadowRoot | null = null;

  // Rebuild shadow DOM content when inputs change
  $effect(() => {
    if (!hostRef) return;

    if (!shadow) {
      shadow = hostRef.attachShadow({ mode: "open" });
    }

    shadow.innerHTML = prepareEmailHtml(html, permissions, isDark);

    // Inline images: resolve <img src="cid:..."> by fetching the part bytes
    // through the provider and swapping in a data: URL. Webview doesn't know
    // the cid: scheme natively. Failures leave the broken-image placeholder.
    const account = accountStore.activeAccount;
    if (account && messageUid != null) {
      const cidImgs = shadow.querySelectorAll<HTMLImageElement>('img[src^="cid:"]');
      for (const img of cidImgs) {
        const cid = img.getAttribute("src")!.slice(4); // strip "cid:"
        invoke<{ mime_type: string; content_b64: string }>("v2_fetch_inline_part", {
          accountId: account.id,
          messageId: messageUid,
          contentId: cid,
        }).then((part) => {
          img.src = `data:${part.mime_type};base64,${part.content_b64}`;
        }).catch((e) => {
          console.warn("[SandboxedEmail] inline part fetch failed for", cid, e);
        });
      }
    }

    // Intercept link clicks → open in system browser
    const links = shadow.querySelectorAll("a[href]");
    console.log("[SandboxedEmail] found", links.length, "links in shadow DOM");
    for (const link of links) {
      link.addEventListener("click", (evt) => {
        evt.preventDefault();
        evt.stopPropagation();
        const href = (link as HTMLAnchorElement).getAttribute("href");
        console.log("[SandboxedEmail] link clicked:", href);
        if (href && (href.startsWith("http://") || href.startsWith("https://"))) {
          invoke("open_url", { url: href }).catch((e) => {
            console.error("[SandboxedEmail] open_url failed:", e);
          });
        }
      });
    }

    // Attach click handlers to blocked placeholders
    const blocked = shadow.querySelectorAll("[data-blocked-src]");
    for (const el of blocked) {
      const htmlEl = el as HTMLElement;
      htmlEl.style.cursor = "pointer";
      htmlEl.addEventListener("click", (evt) => {
        evt.preventDefault();
        evt.stopPropagation();
        const url = htmlEl.getAttribute("data-blocked-src");
        if (!url) return;
        if (window.confirm(`Load external resource?\n${url}`)) {
          const img = document.createElement("img");
          img.src = url;
          img.style.maxWidth = "100%";
          img.style.height = "auto";
          htmlEl.replaceWith(img);
        }
      });
    }
  });
</script>

<div class="email-host" bind:this={hostRef}></div>

<style>
  .email-host {
    overflow-y: auto;
    overflow-x: auto;
    border-radius: 4px;
  }
</style>
