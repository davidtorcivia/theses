export function redrawFocus(lines, active, keep) {
  const focused = lines.indexOf(active);
  const selected = lines.findIndex((line) => line.classList.contains('on'));
  return {
    restore: focused >= 0,
    index: keep ? (focused >= 0 ? focused : selected) : 0,
  };
}
