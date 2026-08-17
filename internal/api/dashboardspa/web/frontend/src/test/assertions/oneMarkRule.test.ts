import { describe, expect, it } from 'vitest';
import { assertAtMostOneMark, assertExactlyOneMark } from './oneMarkRule';

// Unit coverage for the One Mark Rule assertion helper. The helper IS
// the shared mechanical enforcement of DESIGN.md "The One Mark Rule"
// across every route/component test, so its own behavior is pinned by
// these three fixtures: zero / one / two .text-accent elements.

function container(html: string): HTMLDivElement {
  const div = document.createElement('div');
  div.innerHTML = html;
  return div;
}

describe('assertAtMostOneMark', () => {
  it('passes when the container has no .text-accent elements', () => {
    const root = container('<span class="text-fg">calm</span>');
    expect(() => assertAtMostOneMark(root)).not.toThrow();
  });

  it('passes when the container has exactly one .text-accent element', () => {
    const root = container('<a class="text-accent">one mark</a>');
    expect(() => assertAtMostOneMark(root)).not.toThrow();
  });

  it('fails when the container has two .text-accent elements', () => {
    const root = container(
      '<span class="text-accent">one</span><span class="text-accent">two</span>',
    );
    expect(() => assertAtMostOneMark(root)).toThrow();
  });

  it('finds .text-accent on descendants at any depth', () => {
    const root = container(
      '<section><div><p><span class="text-accent">nested</span></p></div></section>',
    );
    expect(() => assertAtMostOneMark(root)).not.toThrow();
    const root2 = container(
      '<section><span class="text-accent">a</span></section>' +
        '<section><span class="text-accent">b</span></section>',
    );
    expect(() => assertAtMostOneMark(root2)).toThrow();
  });
});

describe('assertExactlyOneMark', () => {
  it('fails when the container has no .text-accent elements', () => {
    const root = container('<span class="text-fg">calm</span>');
    expect(() => assertExactlyOneMark(root)).toThrow();
  });

  it('passes when the container has exactly one .text-accent element', () => {
    const root = container('<a class="text-accent">one mark</a>');
    expect(() => assertExactlyOneMark(root)).not.toThrow();
  });

  it('fails when the container has two .text-accent elements', () => {
    const root = container(
      '<span class="text-accent">one</span><span class="text-accent">two</span>',
    );
    expect(() => assertExactlyOneMark(root)).toThrow();
  });

  it('finds .text-accent on descendants at any depth', () => {
    const root = container(
      '<section><div><p><span class="text-accent">nested</span></p></div></section>',
    );
    expect(() => assertExactlyOneMark(root)).not.toThrow();
    const root2 = container(
      '<section><span class="text-accent">a</span></section>' +
        '<section><span class="text-accent">b</span></section>',
    );
    expect(() => assertExactlyOneMark(root2)).toThrow();
  });
});
