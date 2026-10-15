import { describe, expect, it } from 'vitest';
import { scopeOf } from './inbox';

describe('scopeOf', () => {
  it('reads a scope object as key: value pairs', () => {
    expect(scopeOf('{"allow_hosts":["a.example","b.example"],"methods":["GET"]}')).toBe(
      'allow_hosts: a.example, b.example; methods: GET',
    );
  });

  it('says any for no scope, and shows text that is not JSON as it is', () => {
    expect(scopeOf(undefined)).toBe('any');
    expect(scopeOf('{}')).toBe('any');
    expect(scopeOf('/work/x')).toBe('/work/x');
  });
});
