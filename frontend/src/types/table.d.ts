import "@tanstack/react-table";

declare module "@tanstack/react-table" {
  interface ColumnMeta<TData, TValue> {
    align?: "left" | "center" | "right";
    width?: number | string;
    minWidth?: number | string;
    hideable?: boolean;
    sticky?: boolean;
    mono?: boolean;
    priority?: number;
  }
}
