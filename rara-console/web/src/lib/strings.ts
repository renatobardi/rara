// Externalized UI strings (i18n-ready). PT-only in the MVP; adding EN later is swapping this
// object behind a locale switch, not retouching every component. No hardcoded copy in components.
export const t = {
	brand: 'rara',
	nav: {
		overview: 'Visão geral',
		pipeline: 'Pipeline',
		quarantine: 'Quarentena',
		distillations: 'Distillations',
		secTrain: 'Treinar',
		curation: 'Curadoria',
		sources: 'Fontes & Flows',
		providers: 'Providers & Roteamento',
		secSystem: 'Sistema',
		audit: 'Auditoria',
		settings: 'Configurações'
	},
	topbar: {
		search: 'Buscar ou comandar…',
		clean: 'Clean',
		dark: 'Dark'
	},
	status: { online: 'VPC Oracle · online', offline: 'core inacessível' },
	overview: {
		title: 'Visão geral',
		kpiFlows: 'Flows',
		kpiProviders: 'Providers',
		kpiEnabled: 'Providers ativos',
		flowsPanel: 'Flows configurados',
		providersPanel: 'Providers registrados',
		loading: 'Carregando do core ao vivo…',
		error: 'Não foi possível ler a superfície do core.',
		empty: 'Nada seedado ainda.'
	},
	pipeline: {
		title: 'Pipeline',
		loading: 'Carregando pipeline do core ao vivo…',
		error: 'Não foi possível ler o pipeline do core.',
		empty: 'Nenhum item no pipeline ainda.',
		emptyStatus: 'Nenhum item nesta fila.',
		stepsLoading: 'Carregando etapas…',
		stepsEmpty: 'Sem etapas registradas.',
		stepsError: 'Erro ao carregar etapas.',
		colCapability: 'Capability',
		colProvider: 'Provider',
		colStatus: 'Status',
		colAttempts: 'Tentativas',
		statusLabels: {
			discovered: 'Descoberto',
			to_text: 'Para texto',
			distilled: 'Destilado',
			done: 'Concluído',
			filtered: 'Filtrado',
			quarantine: 'Quarentena',
			failed: 'Falhou'
		}
	},
	quarantine: {
		title: 'Quarentena',
		loading: 'Carregando quarentena…',
		error: 'Não foi possível ler a quarentena.',
		empty: 'Nenhum item em quarentena.',
		rescue: 'Me interesso',
		confirmDrop: 'Não me interessa',
		reviewing: 'Processando…',
		reviewError: 'Erro ao processar decisão.',
		retry: 'Tentar novamente'
	},
	distillations: {
		title: 'Distillations',
		loading: 'Carregando distillations…',
		error: 'Não foi possível ler as distillations.',
		empty: 'Nenhuma distillation ainda.',
		detailLoading: 'Carregando…',
		detailError: 'Não foi possível carregar a distillation.',
		thumbUp: '👍',
		thumbDown: '👎',
		feedbackSent: 'Feedback registrado.',
		feedbackError: 'Erro ao registrar feedback.',
		retry: 'Tentar novamente',
		back: '← Voltar'
	}
};
