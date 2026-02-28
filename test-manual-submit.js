const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  let apiCalled = false;

  page.on('request', request => {
    console.log(`>>> ${request.method()} ${request.url()}`);
    if (request.url().includes('/api/')) {
      apiCalled = true;
    }
  });

  page.on('response', async response => {
    if (response.url().includes('/api/')) {
      console.log(`<<< ${response.status()} ${response.url()}`);
      const text = await response.text();
      console.log(`    Response: ${text.substring(0, 200)}`);
    }
  });

  page.on('console', msg => console.log(`[Browser] ${msg.text()}`));

  try {
    console.log('=== Manual htmx Trigger Test ===\n');

    await page.goto('http://jwebhelp.ru:8080/login');
    await page.waitForLoadState('networkidle');

    console.log('\n1. Switching to register tab...');
    await page.click('#register-tab');
    await page.waitForTimeout(1000);

    console.log('\n2. Filling form...');
    await page.fill('#register-username', 'testuser_manual');
    await page.fill('#register-password', 'TestPass123!');
    await page.fill('#register-password-confirm', 'TestPass123!');

    console.log('\n3. Checking htmx...');
    const htmxCheck = await page.evaluate(() => {
      return {
        htmxLoaded: typeof window.htmx !== 'undefined',
        formExists: document.getElementById('register-form') !== null,
        hasHxPost: document.getElementById('register-form')?.getAttribute('hx-post'),
        formVisible: document.getElementById('register-form')?.offsetParent !== null
      };
    });
    console.log('   htmx loaded:', htmxCheck.htmxLoaded);
    console.log('   Form exists:', htmxCheck.formExists);
    console.log('   hx-post attr:', htmxCheck.hasHxPost);
    console.log('   Form visible:', htmxCheck.formVisible);

    console.log('\n4. Trying manual htmx trigger...');
    const manualResult = await page.evaluate(() => {
      const form = document.getElementById('register-form');
      if (!form) return 'Form not found';

      if (typeof window.htmx === 'undefined') return 'htmx not loaded';

      // Try to manually process the form with htmx
      try {
        // Log what we're about to do
        console.log('Calling htmx.process on form...');
        window.htmx.process(form);

        console.log('Triggering submit event...');
        const event = new Event('submit', { bubbles: true, cancelable: true });
        form.dispatchEvent(event);

        return 'Triggered submit event';
      } catch (e) {
        return 'Error: ' + e.message;
      }
    });
    console.log('   Result:', manualResult);

    console.log('\n5. Waiting for API call...');
    await page.waitForTimeout(3000);

    console.log(`\n6. API called: ${apiCalled ? 'YES ✓' : 'NO ✗'}`);

    await page.screenshot({ path: 'test-results/manual-submit.png' });

  } catch (error) {
    console.error('\nError:', error.message);
  } finally {
    await browser.close();
  }
})();
