import { act, fireEvent, render, screen } from '@testing-library/react';

import Search from './Search';

describe('Search', () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });

  afterEach(() => {
    jest.useRealTimers();
  });

  it('debounces typing until 600 ms after the last change', () => {
    const searchFn = jest.fn();
    render(<Search searchFn={searchFn} />);
    const input = screen.getByRole('textbox', { name: 'Resource search' });

    fireEvent.change(input, { target: { value: 'res' } });
    act(() => jest.advanceTimersByTime(300));
    fireEvent.change(input, { target: { value: 'resource' } });
    act(() => jest.advanceTimersByTime(599));
    expect(searchFn).not.toHaveBeenCalled();

    act(() => jest.advanceTimersByTime(1));
    expect(searchFn).toHaveBeenCalledTimes(1);
    expect(searchFn).toHaveBeenCalledWith('resource');
  });

  it('keeps the pending search deadline and uses the latest callback after a render', () => {
    const searchFn = jest.fn();
    const latestSearchFn = jest.fn();
    const { rerender } = render(<Search searchFn={searchFn} />);

    fireEvent.change(screen.getByRole('textbox', { name: 'Resource search' }), {
      target: { value: 'resource' },
    });
    act(() => jest.advanceTimersByTime(300));
    rerender(<Search searchFn={latestSearchFn} />);

    act(() => jest.advanceTimersByTime(299));
    expect(latestSearchFn).not.toHaveBeenCalled();
    act(() => jest.advanceTimersByTime(1));
    expect(searchFn).not.toHaveBeenCalled();
    expect(latestSearchFn).toHaveBeenCalledTimes(1);
    expect(latestSearchFn).toHaveBeenCalledWith('resource');
    expect(screen.getByRole('textbox', { name: 'Resource search' })).toHaveValue('resource');
  });

  it('debounces further typing after the callback changes, including clearing the search', () => {
    const searchFn = jest.fn();
    const latestSearchFn = jest.fn();
    const { rerender } = render(<Search searchFn={searchFn} />);
    const input = screen.getByRole('textbox', { name: 'Resource search' });

    fireEvent.change(input, { target: { value: 'resource' } });
    act(() => jest.advanceTimersByTime(300));
    rerender(<Search searchFn={latestSearchFn} />);
    fireEvent.change(input, { target: { value: '' } });

    act(() => jest.advanceTimersByTime(599));
    expect(latestSearchFn).not.toHaveBeenCalled();
    act(() => jest.advanceTimersByTime(1));
    expect(searchFn).not.toHaveBeenCalled();
    expect(latestSearchFn).toHaveBeenCalledTimes(1);
    expect(latestSearchFn).toHaveBeenCalledWith('');
  });

  it('cancels a pending search when unmounted after the callback changes', () => {
    const searchFn = jest.fn();
    const latestSearchFn = jest.fn();
    const { rerender, unmount } = render(<Search searchFn={searchFn} />);

    fireEvent.change(screen.getByRole('textbox', { name: 'Resource search' }), {
      target: { value: 'resource' },
    });
    rerender(<Search searchFn={latestSearchFn} />);
    unmount();
    act(() => jest.advanceTimersByTime(600));

    expect(searchFn).not.toHaveBeenCalled();
    expect(latestSearchFn).not.toHaveBeenCalled();
  });
});
