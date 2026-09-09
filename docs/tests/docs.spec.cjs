const { test, expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;

test.beforeEach(async ({ page }) => {
  await page.route('https://fonts.googleapis.com/**', route => route.abort());
  await page.route('https://fonts.gstatic.com/**', route => route.abort());
  await page.goto('/');
});

for (const theme of ['light', 'dark']) {
  test(`anchors and accessibility: ${theme}`, async ({ page }) => {
    await page.evaluate(theme => {
      document.documentElement.setAttribute('data-theme', theme);
    }, theme);
    const missing = await page.locator('a[href^="#"]').evaluateAll(links =>
      links.map(link => link.getAttribute('href').slice(1)).filter(id => id && !document.getElementById(id)));
    expect(missing).toEqual([]);
    const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
    expect(results.violations.map(v => ({ id: v.id, targets: v.nodes.map(n => n.target) }))).toEqual([]);
  });
}

test('keyboard search keeps focus, opens results, and restores focus on escape', async ({ page }) => {
  const opener = page.getByRole('button', { name: 'Open search', exact: true });
  await opener.click();
  const input = page.getByRole('combobox', { name: 'Search documentation' });
  await expect(input).toBeFocused();
  await input.fill('security');
  await page.keyboard.press('Tab');
  await expect(input).toBeFocused();
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze();
  expect(results.violations).toEqual([]);
  await page.keyboard.press('Escape');
  await expect(opener).toBeFocused();
  await page.keyboard.press('Control+k');
  await input.fill('kubernetes');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('dialog')).not.toBeVisible();
  await expect(page).toHaveURL(/#.+/);
});

test('mobile navigation and theme persist without horizontal overflow', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const menu = page.getByRole('button', { name: 'Open section menu' });
  await menu.click();
  await expect(menu).toHaveAttribute('aria-expanded', 'true');
  await page.locator('#sidebar a[href="#security"]').click();
  await expect(menu).toHaveAttribute('aria-expanded', 'false');
  await page.locator('#themeToggle').click();
  const selected = await page.evaluate(() => localStorage.getItem('debux-theme'));
  await page.reload();
  expect(await page.evaluate(() => localStorage.getItem('debux-theme'))).toBe(selected);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
