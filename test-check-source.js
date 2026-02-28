const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: false });
  const context = await browser.newContext();
  const page = await context.newPage();

  try {
    console.log('=== Checking Page Source ===\n');

    // Clear cache and reload
    await context.clearCookies();
    await page.goto('http://jwebhelp.ru:8080/login', { waitUntil: 'networkidle' });

    // Get the HTML source
    const html = await page.content();

    // Check for x-show vs :class
    const hasXShow = html.includes('x-show="tab');
    const hasClass = html.includes(':class=');
    const hasHidden = html.includes('hidden');

    console.log('Template check:');
    console.log(`  x-show found: ${hasXShow ? '✗ OLD VERSION' : '✓ not found'}`);
    console.log(`  :class found: ${hasClass ? '✓ NEW VERSION' : '✗ not found'}`);
    console.log(`  .hidden class: ${hasHidden ? '✓ found' : '✗ not found'}`);

    // Switch to register tab and check form attributes
    await page.click('button:has-text("Register")');
    await page.waitForTimeout(1000);

    const formCheck = await page.evaluate(() => {
      const form = document.querySelector('form[hx-post="/api/register"]');
      return {
        hasXShow: form?.hasAttribute('x-show'),
        hasXBind: form?.hasAttribute('x-bind:class') || form?.hasAttribute(':class'),
        classes: form?.className,
        display: form ? window.getComputedStyle(form).display : null
      };
    });

    console.log('\nForm attributes:');
    console.log(`  x-show: ${formCheck.hasXShow ? '✗ still present' : '✓ removed'}`);
    console.log(`  :class binding: ${formCheck.hasXBind ? '✓ present' : '✗ missing'}`);
    console.log(`  CSS classes: ${formCheck.classes}`);
    console.log(`  Display: ${formCheck.display}`);

    // Save source for inspection
    const fs = require('fs');
    fs.writeFileSync('test-results/page-source.html', html);
    console.log('\nPage source saved to: test-results/page-source.html');

    await page.screenshot({ path: 'test-results/source-check.png' });

  } catch (error) {
    console.error('Error:', error.message);
  } finally {
    await browser.close();
  }
})();
