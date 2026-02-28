const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    await page.goto('http://jwebhelp.ru:8080/login');
    await page.waitForLoadState('networkidle');

    // Switch to register tab
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(1000);

    // Check form attributes
    const formInfo = await page.evaluate(() => {
      const form = document.querySelector('form[hx-post="/api/register"]');
      if (!form) return { found: false };

      return {
        found: true,
        isVisible: form.offsetParent !== null,
        display: window.getComputedStyle(form).display,
        hxPost: form.getAttribute('hx-post'),
        hxTarget: form.getAttribute('hx-target'),
        hxSwap: form.getAttribute('hx-swap'),
        xShow: form.getAttribute('x-show'),
        htmxInitialized: form.hasAttribute('hx-target') // htmx adds attributes when initialized
      };
    });

    console.log('Form Info:', JSON.stringify(formInfo, null, 2));

    // Try to manually trigger htmx
    const manualSubmit = await page.evaluate(() => {
      const form = document.querySelector('form[hx-post="/api/register"]');
      if (!form) return 'Form not found';

      // Check if htmx is available
      if (typeof window.htmx === 'undefined') {
        return 'htmx not loaded';
      }

      // Try to trigger htmx request
      try {
        window.htmx.trigger(form, 'submit');
        return 'Triggered htmx.trigger(form, submit)';
      } catch (e) {
        return `Error: ${e.message}`;
      }
    });

    console.log('Manual submit result:', manualSubmit);

    await page.waitForTimeout(3000);

  } catch (error) {
    console.error('Error:', error.message);
  } finally {
    await browser.close();
  }
})();
