// Единственный скрипт сайта: кнопка меню на узких экранах.
// Без него сайт полностью работает — меню просто видно всегда (см. .no-js в CSS).
(function () {
  var btn = document.querySelector('.menu-btn');
  var nav = document.getElementById('nav');
  if (!btn || !nav) return;
  document.documentElement.classList.add('js');
  btn.addEventListener('click', function () {
    var open = btn.getAttribute('aria-expanded') === 'true';
    btn.setAttribute('aria-expanded', String(!open));
    nav.classList.toggle('open', !open);
  });
})();
