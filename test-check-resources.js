const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  const baseUrl = 'http://jwebhelp.ru:8080';
  const failedResources = [];
  const loadedResources = [];

  // Track all resources
  page.on('request', request => {
    console.log(`>>> ${request.method()} ${request.url()}`);
  });

  page.on('response', response => {
    const url = response.url();
    const status = response.status();
    if (status >= 200 && status < 300) {
      loadedResources.push(url);
      console.log(`✓ ${status} ${url}`);
    } else {
      failedResources.push({ url, status });
      console.log(`✗ ${status} ${url}`);
    }
  });

  page.on('requestfailed', request => {
    failedResources.push({ url: request.url(), status: 'FAILED' });
    console.log(`✗ FAILED ${request.url()} - ${request.failure().errorText}`);
  });

  try {
    console.log('\n=== Checking Resource Loading ===\n');

    await page.goto(baseUrl + '/login');
    await page.waitForLoadState('networkidle');
    await page.waitForTimeout(2000);

    console.log('\n=== Summary ===\n');
    console.log(`Loaded: ${loadedResources.length} resources`);
    console.log(`Failed: ${failedResources.length} resources`);

    if (failedResources.length > 0) {
      console.log('\nFailed Resources:');
      failedResources.forEach(r => {
        console.log(`  - ${r.status} ${r.url}`);
      });
    }

    // Check if htmx is loaded
    const htmxLoaded = await page.evaluate(() => {
      return typeof window.htmx !== 'undefined';
    });
    console.log(`\nhtmx loaded: ${htmxLoaded ? '✓ YES' : '✗ NO'}`);

    // Check if Alpine is loaded
    const alpineLoaded = await page.evaluate(() => {
      return typeof window.Alpine !== 'undefined';
    });
    console.log(`Alpine loaded: ${alpineLoaded ? '✓ YES' : '✗ NO'}`);

    await page.screenshot({ path: 'test-results/resource-check.png' });

  } catch (error) {
    console.error('\n❌ ERROR:', error.message);
  } finally {
    await browser.close();
  }
})();
