<script lang="ts">
  import { prepareEmailHtml, type ContentPermissions } from "../utils/html";

  interface Props {
    html: string;
    isDark: boolean;
    permissions: ContentPermissions;
  }
  let { html, isDark, permissions }: Props = $props();

  let hostRef = $state<HTMLDivElement | null>(null);
  let shadow: ShadowRoot | null = null;

  // Rebuild shadow DOM content when inputs change
  $effect(() => {
    if (!hostRef) return;

    if (!shadow) {
      shadow = hostRef.attachShadow({ mode: "open" });
    }

    shadow.innerHTML = prepareEmailHtml(html, permissions, isDark);

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
