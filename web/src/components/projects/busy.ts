/**
 * Classes for a control that is unavailable while the form is busy. It
 * stays enabled (and keeps focus) but ignores pointers and looks disabled;
 * handlers check `busy` themselves.
 */
export const busyClass = "aria-disabled:pointer-events-none aria-disabled:opacity-50"
