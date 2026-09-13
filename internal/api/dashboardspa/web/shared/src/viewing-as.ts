/** Frontend "viewing as" context state. Default identity is the operator. */
export interface ViewingAs {
  alias: string;
  /** True iff alias is the operator alias, the sole identity that can send. */
  isOperator: boolean;
}
