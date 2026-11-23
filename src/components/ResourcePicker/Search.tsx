import { debounce } from 'lodash';
import { useEffect, useMemo, useRef, useState } from 'react';

import { t } from '@grafana/i18n';
import { Icon, Input } from '@grafana/ui';

import { selectors } from '../../e2e/selectors';

const Search = ({ searchFn }: { searchFn: (searchPhrase: string) => void }) => {
  const [searchFilter, setSearchFilter] = useState('');

  const latestSearchFn = useRef(searchFn);
  useEffect(() => {
    latestSearchFn.current = searchFn;
  }, [searchFn]);

  const debouncedSearch = useMemo(
    () => debounce((searchPhrase: string) => latestSearchFn.current(searchPhrase), 600),
    []
  );
  useEffect(() => {
    return () => {
      // Stop the invocation of the debounced function after unmounting
      debouncedSearch.cancel();
    };
  }, [debouncedSearch]);

  return (
    <Input
      aria-label={t('components.search.aria-label-resource-search', 'Resource search')}
      prefix={<Icon name="search" />}
      value={searchFilter}
      onChange={(event) => {
        const searchPhrase = event.currentTarget.value;
        setSearchFilter(searchPhrase);
        debouncedSearch(searchPhrase);
      }}
      placeholder={t('components.search.placeholder-resource-search', 'Search for a resource')}
      data-testid={selectors.components.queryEditor.resourcePicker.search.input}
      style={{ marginBottom: '10px' }}
    />
  );
};

export default Search;
