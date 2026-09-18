// The art panel from the mockup's auth pages: a slow cross-fade between frames
// with the tear effect fired in bursts. --img carries the frame the tear layers
// sample, and is set here rather than in the markup because the CSP has no
// 'unsafe-inline' in style-src.
(function () {
  var art = document.querySelector('.art');
  if (!art) return;

  var imgs = art.querySelectorAll('img');
  var frames = (art.dataset.frames || '').split(',').filter(Boolean);
  var base = art.dataset.base || '';
  var i = 1;

  function sample(img) {
    art.style.setProperty('--img', 'url(' + img.src + ')');
  }
  if (imgs.length) sample(imgs[0]);

  function burst() {
    art.classList.add('live');
    setTimeout(function () { art.classList.remove('live'); }, 500 + Math.random() * 500);
  }
  function tick() {
    burst();
    setTimeout(tick, 2600 + Math.random() * 3400);
  }
  setTimeout(tick, 900);
  art.addEventListener('pointerenter', function () { art.classList.add('live'); });
  art.addEventListener('pointerleave', function () { art.classList.remove('live'); });

  if (imgs.length > 1 && frames.length > 1) {
    setInterval(function () {
      var want = base + frames[i % frames.length] + '.jpg';
      var cur = imgs[(i + 1) % 2];
      var next = imgs[i % 2];
      i++;
      var show = function () {
        next.classList.remove('out');
        cur.classList.add('out');
        sample(next);
      };
      if (next.getAttribute('src') === want) {
        show();
      } else {
        next.onload = show;
        next.src = want;
      }
    }, 8000);
  }
})();

// Submitting says so. The form still posts; this only changes the label.
(function () {
  document.querySelectorAll('form').forEach(function (form) {
    form.addEventListener('submit', function () {
      var b = form.querySelector('.btn[data-busy]');
      if (b) b.textContent = b.dataset.busy;
    });
  });
})();
