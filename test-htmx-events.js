const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  const baseUrl = 'http://jwebhelp.ru:8080';
  const testUsername = `testuser_${Date.now()}`;
  const testPassword = 'TestPass123!';

  // Track all API requests
  const apiRequests = [];
  page.on('request', request => {
    if (request.url().includes('/api/')) {
      apiRequests.push({
        method: request.method(),
        url: request.url(),
        postData: request.postData()
      });
      console.log(`\n>>> API REQUEST: ${request.method()} ${request.url()}`);
      if (request.postData()) {
        console.log(`    Data: ${request.postData()}`);
      }
    }
  });

  page.on('response', async response => {
    if (response.url().includes('/api/')) {
      console.log(`\n<<< API RESPONSE: ${response.status()} ${response.url()}`);
      try {
        const text = await response.text();
        console.log(`    Body: ${text}`);
      } catch (e) {}
    }
  });

  // Log all console messages including htmx events
  page.on('console', msg => {
    const text = msg.text();
    if (text.includes('htmx') || text.includes('Failed') || text.includes('Error')) {
      console.log(`[Console] ${text}`);
    }
  });

  try {
    console.log('=== Testing htmx Form Submission ===\n');

    // Navigate to login page
    console.log('1. Loading page...');
    await page.goto(baseUrl + '/login');
    await page.waitForLoadState('networkidle');

    // Add htmx event listeners via browser console
    await page.evaluate(() => {
      // Log all htmx events
      document.body.addEventListener('htmx:configRequest', (evt) => {
        console.log('htmx:configRequest', evt.detail);
      });
      document.body.addEventListener('htmx:beforeRequest', (evt) => {
        console.log('htmx:beforeRequest', evt.detail.xhr.responseURL);
      });
      document.body.addEventListener('htmx:afterRequest', (evt) => {
        console.log('htmx:afterRequest', {
          successful: evt.detail.successful,
          url: evt.detail.xhr.responseURL,
          status: evt.detail.xhr.status,
          response: evt.detail.xhr.responseText
        });
      });
      document.body.addEventListener('htmx:responseError', (evt) => {
        console.log('htmx:responseError', evt.detail);
      });
    });

    // Switch to registration tab
    console.log('\n2. Switching to registration tab...');
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(500);

    // Fill form
    console.log('\n3. Filling form...');
    await page.fill('#register-username', testUsername);
    await page.fill('#register-password', testPassword);
    await page.fill('#register-password-confirm', testPassword);

    // Submit form
    console.log('\n4. Submitting form...');
    console.log(`   Username: ${testUsername}`);
    await page.click('button:has-text("Register")');

    // Wait for response
    console.log('\n5. Waiting for response...');
    await page.waitForTimeout(5000);

    console.log('\n=== RESULTS ===');
    console.log(`API Requests sent: ${apiRequests.length}`);
    if (apiRequests.length === 0) {
      console.log('❌ NO API REQUESTS WERE SENT!');
      console.log('   This means htmx is not intercepting the form submission.');
    } else {
      console.log('✓ API requests sent successfully');
      apiRequests.forEach(req => {
        console.log(`  - ${req.method} ${req.url}`);
      });
    }

    console.log(`\nFinal URL: ${page.url()}`);
    await page.screenshot({ path: 'test-results/htmx-test.png' });

  } catch (error) {
    console.error('\n❌ ERROR:', error.message);
  } finally {
    await browser.close();
  }
})();
